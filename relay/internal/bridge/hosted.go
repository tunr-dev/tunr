package bridge

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tunr-dev/tunr/relay/internal/audit"
	"github.com/tunr-dev/tunr/relay/internal/db"
	"github.com/tunr-dev/tunr/relay/internal/logger"
)

// hosted.go — agents that run on tunr (P7).
//
// Identity: every deploy rotates a bootstrap secret, handed to the container
// as TUNR_AGENT_BOOTSTRAP. The tunr CLI inside exchanges it at
// POST /api/v1/bridge/agent/token for a 1-hour agent token and refreshes
// before expiry, so no long-lived *agent token* sits in the container. The
// bootstrap itself is long-lived (until the next deploy or until the agent is
// disabled) — it is a credential, scoped to minting tokens for this one agent.
//
// Reachability: agent apps are not public. The relay answers every request to
// <agent>.<domain> itself unless it is a correctly signed webhook
// (/__tunr/hooks/<name>); cron triggers are dispatched in-process. Both reach
// the agent with X-Tunr-Trigger and a relay signature it can check with
// TUNR_TRIGGER_SECRET.

// AgentAppDeployer is the control plane's hosted-agent deploy (wired in main).
type AgentAppDeployer interface {
	DeployAgentApp(ctx context.Context, userID, claimPlan, name string, port int, env map[string]string,
		volumeMB int, src io.Reader, send func(map[string]any)) (appID string, err error)
}

// Dispatcher delivers a request to an app through the normal cloud proxy
// (waking it if needed) and returns the status code.
type Dispatcher func(ctx context.Context, req *http.Request) int

const (
	hostedTokenTTL     = time.Hour
	hookPrefix         = "/__tunr/hooks/"
	maxHookBody        = 1 << 20
	hookSkew           = 5 * time.Minute
	triggerTimeout     = 15 * time.Minute
	hostedCacheRefresh = 30 * time.Second
)

var triggerPathRe = regexp.MustCompile(`^/[A-Za-z0-9._~/-]{0,99}$`)

func randSecret(prefix string) string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return prefix + base64.RawURLEncoding.EncodeToString(b)
}

func sha256hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// SignTrigger signs a relay → agent trigger: "t=<unix>,v1=<hex hmac(secret, "t.body")>".
// Same shape as SignWebhook, so agents use one verifier.
func SignTrigger(secret string, t time.Time, body []byte) string { return SignWebhook(secret, t, body) }

// verifySig checks a "t=…,v1=…" header against secret within hookSkew.
func verifySig(secret, header string, body []byte, now time.Time) bool {
	var ts, sig string
	for _, part := range strings.Split(header, ",") {
		k, v, _ := strings.Cut(strings.TrimSpace(part), "=")
		switch k {
		case "t":
			ts = v
		case "v1":
			sig = v
		}
	}
	unix, err := strconv.ParseInt(ts, 10, 64)
	if err != nil || sig == "" || secret == "" {
		return false
	}
	if d := now.Sub(time.Unix(unix, 0)); d > hookSkew || d < -hookSkew {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts + "."))
	mac.Write(body)
	want := hex.EncodeToString(mac.Sum(nil))
	return subtle.ConstantTimeCompare([]byte(want), []byte(sig)) == 1
}

// ── hosted app cache ─────────────────────────────────────────────────────────

type hostedCache struct {
	mu     sync.RWMutex
	byName map[string]db.HostedApp
	loaded time.Time
}

func (s *Server) hostedApp(ctx context.Context, name string) (db.HostedApp, bool) {
	c := s.hosted
	c.mu.RLock()
	fresh := time.Since(c.loaded) < hostedCacheRefresh
	app, ok := c.byName[name]
	c.mu.RUnlock()
	if fresh {
		return app, ok
	}
	s.refreshHosted(ctx)
	c.mu.RLock()
	defer c.mu.RUnlock()
	app, ok = c.byName[name]
	return app, ok
}

func (s *Server) refreshHosted(ctx context.Context) {
	cctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	apps, err := s.db.ListHostedApps(cctx)
	if err != nil {
		logger.Warn("bridge: hosted apps: %v", err)
		return // keep serving the last known set
	}
	m := make(map[string]db.HostedApp, len(apps))
	for _, a := range apps {
		m[a.Name] = a
	}
	s.hosted.mu.Lock()
	s.hosted.byName, s.hosted.loaded = m, time.Now()
	s.hosted.mu.Unlock()
}

// ── routes ───────────────────────────────────────────────────────────────────

func (s *Server) registerHosted(mux *http.ServeMux) {
	h := func(pattern string, fn http.HandlerFunc) { mux.Handle(pattern, s.humanAuth(fn)) }
	const p = "/api/v1/bridge"
	h("POST "+p+"/orgs/{org}/agents/{id}/deploy", s.deployAgent)
	h("GET "+p+"/orgs/{org}/agents/{id}/triggers", s.listTriggers)
	h("POST "+p+"/orgs/{org}/agents/{id}/triggers", s.addTrigger)
	h("DELETE "+p+"/orgs/{org}/triggers/{tid}", s.deleteTrigger)
	mux.HandleFunc("POST "+p+"/agent/token", s.exchangeBootstrap)
}

// deployAgent: multipart meta {name, port, env, volume_mb} + source (tar.gz),
// streamed back as SSE like /v1/deploy.
func (s *Server) deployAgent(w http.ResponseWriter, r *http.Request) {
	m := s.admin(w, r)
	if m == nil {
		return
	}
	if !m.IsVendor() {
		apiError(w, http.StatusBadRequest, "org_kind", "only vendor orgs run agents")
		return
	}
	if s.deployer == nil {
		apiError(w, http.StatusServiceUnavailable, "unavailable", "hosted agents are not available on this relay")
		return
	}
	agent, err := s.db.GetAgent(r.Context(), m.ID, r.PathValue("id"))
	if err != nil {
		s.dbError(w, err, "agent")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 55<<20)
	if err := r.ParseMultipartForm(8 << 20); err != nil {
		apiError(w, http.StatusBadRequest, "invalid_request", "multipart meta + source expected")
		return
	}
	var meta struct {
		Name     string            `json:"name"`
		Port     int               `json:"port"`
		Env      map[string]string `json:"env"`
		VolumeMB int               `json:"volume_mb"`
	}
	if err := strictUnmarshal(json.RawMessage(r.FormValue("meta")), &meta); err != nil {
		apiError(w, http.StatusBadRequest, "invalid_request", "bad meta: "+err.Error())
		return
	}
	if meta.Name == "" {
		meta.Name = agent.Name
	}
	for k := range meta.Env {
		if strings.HasPrefix(strings.ToUpper(k), "TUNR_") {
			apiError(w, http.StatusBadRequest, "reserved_env", k+": TUNR_* variables are set by tunr")
			return
		}
	}
	if meta.VolumeMB < 0 || meta.VolumeMB > 1<<20 {
		apiError(w, http.StatusBadRequest, "invalid_volume", "volume_mb out of range")
		return
	}
	src, _, err := r.FormFile("source")
	if err != nil {
		apiError(w, http.StatusBadRequest, "invalid_request", "missing source")
		return
	}
	defer src.Close()

	// Rotate credentials before the new container can start and use them.
	bootstrap := randSecret("tunr_boot_")
	triggerSecret := randSecret("tsec_")
	if err := s.db.SetAgentBootstrap(r.Context(), m.ID, agent.ID, sha256hex(bootstrap), triggerSecret); err != nil {
		s.dbError(w, err, "agent")
		return
	}
	env := map[string]string{}
	for k, v := range meta.Env {
		env[k] = v
	}
	env["TUNR_AGENT_BOOTSTRAP"] = bootstrap
	env["TUNR_RELAY_URL"] = s.cfg.PublicURL
	env["TUNR_TRIGGER_SECRET"] = triggerSecret
	env["TUNR_AGENT_ID"] = agent.ID
	env["TUNR_AGENT_NAME"] = agent.Name
	if strings.HasPrefix(s.cfg.PublicURL, "http://") {
		// A relay without TLS (development, some self-hosting): the agent
		// reaches it on the internal network, so let the CLI accept it.
		env["TUNR_BRIDGE_INSECURE_RELAY"] = "1"
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		apiError(w, http.StatusInternalServerError, "internal", "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	send := func(v map[string]any) {
		b, _ := json.Marshal(v)
		fmt.Fprintf(w, "data: %s\n\n", b)
		flusher.Flush()
	}
	u := userFrom(r)
	appID, err := s.deployer.DeployAgentApp(r.Context(), u.ID, u.Plan, meta.Name, meta.Port, env, meta.VolumeMB, src, send)
	if err != nil {
		send(map[string]any{"event": "failed", "error": err.Error()})
		return
	}
	if err := s.db.LinkAgentApp(r.Context(), m.ID, agent.ID, appID, meta.VolumeMB); err != nil {
		send(map[string]any{"event": "failed", "error": "link agent: " + err.Error()})
		return
	}
	s.refreshHosted(r.Context())
	s.audit(r, m.ID, audit.ActionBridgeAgentDeployed, map[string]any{"agent_id": agent.ID, "app_id": appID, "name": meta.Name})
	send(map[string]any{"event": "live", "agent_id": agent.ID, "app": meta.Name,
		"detail": "running on tunr; reachable only through triggers"})
}

// exchangeBootstrap trades a hosted agent's bootstrap secret for a 1h agent token.
func (s *Server) exchangeBootstrap(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	var in struct {
		Bootstrap string `json:"bootstrap"`
	}
	if !decode(w, r, &in) {
		return
	}
	if ok, wait := s.apiLimit.allow("b:"+sha256hex(in.Bootstrap)[:16], time.Now()); !ok {
		rateLimited(w, wait)
		return
	}
	if !strings.HasPrefix(in.Bootstrap, "tunr_boot_") {
		apiError(w, http.StatusUnauthorized, "invalid_bootstrap", "invalid bootstrap secret")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), dbTimeout)
	defer cancel()
	a, err := s.db.AgentByBootstrap(ctx, sha256hex(in.Bootstrap))
	if err != nil {
		apiError(w, http.StatusUnauthorized, "invalid_bootstrap", "invalid or rotated bootstrap secret")
		return
	}
	rec, err := s.db.CreateAgentToken(ctx, a.OrgID, a.ID, "", time.Now().Add(hostedTokenTTL))
	if err != nil {
		s.dbError(w, err, "token")
		return
	}
	tok, err := s.jwt.IssueAgent(a.OrgID, a.ID, rec.ID, rec.ExpiresAt)
	if err != nil {
		s.dbError(w, err, "token")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"token": tok, "expires_at": rec.ExpiresAt, "agent_id": a.ID})
}

// ── triggers ─────────────────────────────────────────────────────────────────

func (s *Server) listTriggers(w http.ResponseWriter, r *http.Request) {
	m := s.member(w, r)
	if m == nil {
		return
	}
	ts, err := s.db.ListAgentTriggers(r.Context(), m.ID, r.PathValue("id"))
	if err != nil {
		s.dbError(w, err, "triggers")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"triggers": ts})
}

func (s *Server) addTrigger(w http.ResponseWriter, r *http.Request) {
	m := s.admin(w, r)
	if m == nil {
		return
	}
	var in struct{ Kind, Name, Spec, Path string }
	if !decode(w, r, &in) {
		return
	}
	if in.Path == "" {
		in.Path = "/run"
	}
	if !triggerPathRe.MatchString(in.Path) || strings.Contains(in.Path, "..") || strings.HasPrefix(in.Path, "/__tunr") {
		apiError(w, http.StatusBadRequest, "invalid_path", "path must look like /run")
		return
	}
	if !validSlug(in.Name) {
		apiError(w, http.StatusBadRequest, "invalid_name", "name: 3-32 chars, lowercase letters, digits, hyphens")
		return
	}
	secret := ""
	switch in.Kind {
	case "cron":
		if _, err := ParseCron(in.Spec); err != nil {
			apiError(w, http.StatusBadRequest, "invalid_cron", err.Error())
			return
		}
	case "webhook":
		in.Spec = ""
		secret = randSecret("whsec_")
	default:
		apiError(w, http.StatusBadRequest, "invalid_kind", "kind must be cron or webhook")
		return
	}
	t, err := s.db.UpsertTrigger(r.Context(), m.ID, r.PathValue("id"), in.Kind, in.Name, in.Spec, in.Path, secret)
	if err != nil {
		s.dbError(w, err, "trigger (deploy the agent first)")
		return
	}
	s.audit(r, m.ID, audit.ActionBridgeTriggerChanged, map[string]any{"trigger_id": t.ID, "kind": t.Kind, "name": t.Name, "spec": t.Spec})
	out := map[string]any{"trigger": t}
	if t.Kind == "webhook" {
		ag, _ := s.db.GetAgent(r.Context(), m.ID, r.PathValue("id"))
		app := ""
		if ag != nil {
			s.refreshHosted(r.Context())
			for _, h := range s.hostedSnapshot() {
				if h.AgentID == ag.ID {
					app = h.Name
				}
			}
		}
		out["url"] = fmt.Sprintf("https://%s.%s%s%s", app, s.cfg.Issuer, hookPrefix, t.Name)
		out["secret"] = secret
		out["signature"] = `X-Tunr-Signature: t=<unix>,v1=<hex HMAC-SHA256(secret, "<unix>.<body>")>`
	}
	writeJSON(w, http.StatusCreated, out)
}

func (s *Server) hostedSnapshot() []db.HostedApp {
	s.hosted.mu.RLock()
	defer s.hosted.mu.RUnlock()
	out := make([]db.HostedApp, 0, len(s.hosted.byName))
	for _, a := range s.hosted.byName {
		out = append(out, a)
	}
	return out
}

func (s *Server) deleteTrigger(w http.ResponseWriter, r *http.Request) {
	m := s.admin(w, r)
	if m == nil {
		return
	}
	if err := s.db.DeleteTrigger(r.Context(), m.ID, r.PathValue("tid")); err != nil {
		s.dbError(w, err, "trigger")
		return
	}
	s.audit(r, m.ID, audit.ActionBridgeTriggerChanged, map[string]any{"trigger_id": r.PathValue("tid"), "deleted": true})
	w.WriteHeader(http.StatusNoContent)
}

func validSlug(s string) bool { return slugRe.MatchString(s) && !strings.Contains(s, "--") }

// ── delivery ─────────────────────────────────────────────────────────────────

// triggerRequest builds the request an agent receives for a trigger.
func (s *Server) triggerRequest(ctx context.Context, app db.HostedApp, kind, name, path, contentType string, body []byte) *http.Request {
	// Never sent over the network: handed to the in-process cloud proxy, which
	// routes by Host to the agent's own container. #nosec G704
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "https://"+app.Name+"."+s.cfg.Issuer+path, bytes.NewReader(body))
	req.Host = app.Name + "." + s.cfg.Issuer
	if contentType == "" {
		contentType = "application/json"
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("User-Agent", "tunr-trigger/1")
	req.Header.Set("X-Tunr-Trigger", kind+":"+name)
	req.Header.Set("X-Tunr-Trigger-Signature", SignTrigger(app.TriggerSecret, time.Now(), body))
	req.RemoteAddr = "127.0.0.1:0" // in-process, not a client
	return req
}

// WrapProxy guards agent apps: everything addressed to <agent>.<domain> is
// answered here unless it is a signed webhook, which is forwarded to the
// agent's trigger path. Other hosts pass straight through to next.
func (s *Server) WrapProxy(next http.Handler) http.Handler {
	if !s.Enabled() {
		return next
	}
	s.dispatchVia = func(ctx context.Context, req *http.Request) int {
		rec := &statusWriter{header: http.Header{}}
		next.ServeHTTP(rec, req)
		return rec.status
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		sub, ok := strings.CutSuffix(strings.ToLower(host), "."+s.cfg.Issuer)
		if !ok || strings.Contains(sub, ".") {
			next.ServeHTTP(w, r)
			return
		}
		app, isAgent := s.hostedApp(r.Context(), sub)
		if !isAgent {
			next.ServeHTTP(w, r)
			return
		}
		if !app.Active {
			http.Error(w, "this hosted agent is disabled", http.StatusServiceUnavailable)
			return
		}
		name, isHook := strings.CutPrefix(r.URL.Path, hookPrefix)
		if !isHook || r.Method != http.MethodPost {
			http.Error(w, "this is a hosted agent; it only accepts signed triggers", http.StatusNotFound)
			return
		}
		s.serveHook(w, r, app, name)
	})
}

func (s *Server) serveHook(w http.ResponseWriter, r *http.Request, app db.HostedApp, name string) {
	ctx, cancel := context.WithTimeout(r.Context(), dbTimeout)
	trig, err := s.db.WebhookTrigger(ctx, app.Name, name)
	cancel()
	if err != nil {
		http.Error(w, "unknown hook", http.StatusNotFound)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxHookBody))
	if err != nil {
		http.Error(w, "body too large", http.StatusRequestEntityTooLarge)
		return
	}
	if !verifySig(trig.Secret, r.Header.Get("X-Tunr-Signature"), body, time.Now()) {
		http.Error(w, "bad or missing X-Tunr-Signature", http.StatusUnauthorized)
		return
	}
	status := s.deliver(r.Context(), app, trig, r.Header.Get("Content-Type"), body)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(map[string]any{"delivered": status > 0 && status < 500, "agent_status": status})
}

// deliver sends a trigger to the agent and audits it.
func (s *Server) deliver(ctx context.Context, app db.HostedApp, t *db.Trigger, contentType string, body []byte) int {
	if s.dispatchVia == nil {
		return 0
	}
	dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), triggerTimeout)
	defer cancel()
	start := time.Now()
	status := s.dispatchVia(dctx, s.triggerRequest(dctx, app, t.Kind, t.Name, t.Path, contentType, body))
	actx, acancel := context.WithTimeout(context.Background(), dbTimeout)
	defer acancel()
	if err := s.db.AuditOrg(actx, app.OrgID, "", audit.ActionBridgeTriggerFired, map[string]any{
		"trigger_id": t.ID, "kind": t.Kind, "name": t.Name, "agent_id": app.AgentID,
		"status": status, "duration_ms": time.Since(start).Milliseconds(),
	}); err != nil {
		logger.Warn("bridge audit: %v", err)
	}
	return status
}

// statusWriter records only the status of an in-process dispatch.
type statusWriter struct {
	header http.Header
	status int
}

func (w *statusWriter) Header() http.Header { return w.header }
func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return len(b), nil
}
func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
}

// ── cron ─────────────────────────────────────────────────────────────────────

var cronInterval = 20 * time.Second

// StartCron fires due cron triggers. Each (trigger, minute) is claimed in the
// database first, so several relay instances never fire the same minute twice.
func (s *Server) StartCron(ctx context.Context) {
	if !s.Enabled() {
		return
	}
	go func() {
		t := time.NewTicker(cronInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-t.C:
				s.cronTick(ctx, now)
			}
		}
	}()
}

func (s *Server) cronTick(ctx context.Context, now time.Time) {
	minute := now.UTC().Truncate(time.Minute)
	cctx, cancel := context.WithTimeout(ctx, dbTimeout)
	ts, err := s.db.ListCronTriggers(cctx)
	cancel()
	if err != nil {
		logger.Warn("bridge cron: %v", err)
		return
	}
	for i := range ts {
		t := ts[i]
		c, err := ParseCron(t.Spec)
		if err != nil || !c.Matches(minute) {
			continue
		}
		cctx, cancel := context.WithTimeout(ctx, dbTimeout)
		won, err := s.db.ClaimCronFire(cctx, t.ID, minute)
		cancel()
		if err != nil || !won {
			continue
		}
		app, ok := s.appByID(ctx, t.AppID)
		if !ok {
			continue
		}
		body, _ := json.Marshal(map[string]any{"trigger": t.Name, "kind": "cron", "scheduled_for": minute})
		go s.deliver(ctx, app, &t, "application/json", body)
	}
}

func (s *Server) appByID(ctx context.Context, appID string) (db.HostedApp, bool) {
	s.refreshHosted(ctx)
	for _, a := range s.hostedSnapshot() {
		if a.AppID == appID && a.Active {
			return a, true
		}
	}
	return db.HostedApp{}, false
}
