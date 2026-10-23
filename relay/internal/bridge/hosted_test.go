package bridge

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tunr-dev/tunr/relay/internal/db"
)

// fakeDeployer records the env and creates the app row like the control plane.
type fakeDeployer struct {
	d   *db.DB
	mu  sync.Mutex
	env map[string]string
	n   int
}

func (f *fakeDeployer) DeployAgentApp(ctx context.Context, userID, plan, name string, port int, env map[string]string,
	vol int, src io.Reader, send func(map[string]any)) (string, error) {
	f.mu.Lock()
	f.env, f.n = env, f.n+1
	f.mu.Unlock()
	app, _, err := f.d.GetOrCreateAppByName(ctx, userID, name, "a_"+name, "edge", 8080)
	if err != nil {
		return "", err
	}
	send(map[string]any{"event": "building"})
	return app.ID, nil
}

func (e *p4) deployAgent(tok string, meta map[string]any, want int) string {
	e.t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	mb, _ := json.Marshal(meta)
	_ = mw.WriteField("meta", string(mb))
	fw, _ := mw.CreateFormFile("source", "src.tar.gz")
	_, _ = fw.Write([]byte("fake"))
	_ = mw.Close()
	req, _ := http.NewRequest("POST", e.srv.URL+"/api/v1/bridge/orgs/vendor-co/agents/"+e.agentID+"/deploy", &buf)
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != want {
		e.t.Fatalf("deploy = %d %s, want %d", resp.StatusCode, b, want)
	}
	return string(b)
}

func newHostedEnv(t *testing.T) (*p4, *fakeDeployer, *[]*http.Request) {
	e := newP4(t)
	fd := &fakeDeployer{d: e.d}
	e.s.deployer = fd
	var got []*http.Request
	var mu sync.Mutex
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		r2 := r.Clone(context.Background())
		r2.Body = io.NopCloser(bytes.NewReader(b))
		mu.Lock()
		got = append(got, r2)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	})
	wrapped := e.s.WrapProxy(next)
	e.s.cfg.Issuer = "tunr.test"
	mux := http.NewServeMux()
	e.s.RegisterRoutes(mux)
	mux.Handle("/", wrapped)
	e.srv.Config.Handler = mux
	return e, fd, &got
}

func TestHostedDeployAndBootstrap(t *testing.T) {
	e, fd, _ := newHostedEnv(t)
	e.deployAgent(e.member, map[string]any{"name": "bot"}, 404) // not a vendor member
	e.deployAgent(e.vendor, map[string]any{"name": "bot", "env": map[string]string{"TUNR_AGENT_BOOTSTRAP": "x"}}, 400)
	out := e.deployAgent(e.vendor, map[string]any{"name": "bot", "env": map[string]string{"OPENAI_KEY": "k"}}, 200)
	if !strings.Contains(out, `"event":"live"`) {
		t.Fatalf("deploy stream: %s", out)
	}
	boot := fd.env["TUNR_AGENT_BOOTSTRAP"]
	if !strings.HasPrefix(boot, "tunr_boot_") || fd.env["TUNR_TRIGGER_SECRET"] == "" || fd.env["OPENAI_KEY"] != "k" ||
		fd.env["TUNR_AGENT_ID"] != e.agentID || fd.env["TUNR_RELAY_URL"] == "" {
		t.Fatalf("env = %v", fd.env)
	}

	exchange := func(b string) (int, string) {
		resp, err := http.Post(e.srv.URL+"/api/v1/bridge/agent/token", "application/json", strings.NewReader(`{"bootstrap":"`+b+`"}`))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out struct{ Token string }
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out.Token
	}
	code, tok := exchange(boot)
	if code != 200 || tok == "" {
		t.Fatalf("exchange: %d", code)
	}
	var me map[string]any
	e.call(tok, "GET", "/agent/me", nil, 200, &me)
	if code, _ := exchange("tunr_boot_forged"); code != 401 {
		t.Errorf("forged bootstrap: %d", code)
	}
	// Redeploy rotates the bootstrap; the old one stops working.
	e.deployAgent(e.vendor, map[string]any{"name": "bot"}, 200)
	if code, _ := exchange(boot); code != 401 {
		t.Errorf("old bootstrap after redeploy: %d", code)
	}
	if code, _ := exchange(fd.env["TUNR_AGENT_BOOTSTRAP"]); code != 200 {
		t.Errorf("new bootstrap: %d", code)
	}
	// Disabling the agent kills the bootstrap too.
	e.call(e.vendor, "PATCH", "/orgs/vendor-co/agents/"+e.agentID, map[string]string{"status": "disabled"}, 200, nil)
	if code, _ := exchange(fd.env["TUNR_AGENT_BOOTSTRAP"]); code != 401 {
		t.Errorf("bootstrap of a disabled agent: %d", code)
	}
}

func TestHostedTriggersAndPrivacy(t *testing.T) {
	e, fd, got := newHostedEnv(t)
	e.deployAgent(e.vendor, map[string]any{"name": "bot"}, 200)
	base := "/orgs/vendor-co/agents/" + e.agentID + "/triggers"
	e.call(e.vendor, "POST", base, map[string]string{"kind": "cron", "name": "nightly", "spec": "61 * * * *"}, 400, nil)
	e.call(e.vendor, "POST", base, map[string]string{"kind": "cron", "name": "nightly", "spec": "0 7 * * *", "path": "/../etc"}, 400, nil)
	e.call(e.vendor, "POST", base, map[string]string{"kind": "cron", "name": "nightly", "spec": "* * * * *"}, 201, nil)
	var hook struct {
		URL, Secret string
	}
	e.call(e.vendor, "POST", base, map[string]string{"kind": "webhook", "name": "new-invoice", "path": "/hooks/invoice"}, 201, &hook)
	if !strings.HasSuffix(hook.URL, "bot.tunr.test/__tunr/hooks/new-invoice") || !strings.HasPrefix(hook.Secret, "whsec_") {
		t.Fatalf("webhook = %+v", hook)
	}

	send := func(path, sig string, body []byte) int {
		req, _ := http.NewRequest("POST", e.srv.URL+path, bytes.NewReader(body))
		req.Host = "bot.tunr.test"
		if sig != "" {
			req.Header.Set("X-Tunr-Signature", sig)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	body := []byte(`{"invoice":42}`)
	// The agent is not public.
	if c := send("/", "", nil); c != 404 {
		t.Errorf("public request to agent: %d", c)
	}
	if c := send("/hooks/invoice", "", body); c != 404 {
		t.Errorf("direct request to the trigger path: %d", c)
	}
	if c := send("/__tunr/hooks/new-invoice", "", body); c != 401 {
		t.Errorf("unsigned hook: %d", c)
	}
	if c := send("/__tunr/hooks/new-invoice", SignWebhook("whsec_wrong", time.Now(), body), body); c != 401 {
		t.Errorf("wrongly signed hook: %d", c)
	}
	if c := send("/__tunr/hooks/new-invoice", SignWebhook(hook.Secret, time.Now().Add(-10*time.Minute), body), body); c != 401 {
		t.Errorf("stale hook: %d", c)
	}
	if c := send("/__tunr/hooks/new-invoice", SignWebhook(hook.Secret, time.Now(), body), body); c != 202 {
		t.Fatalf("signed hook: %d", c)
	}
	if len(*got) != 1 {
		t.Fatalf("dispatched %d requests", len(*got))
	}
	r := (*got)[0]
	b, _ := io.ReadAll(r.Body)
	if r.URL.Path != "/hooks/invoice" || r.Header.Get("X-Tunr-Trigger") != "webhook:new-invoice" || string(b) != `{"invoice":42}` ||
		!verifySig(fd.env["TUNR_TRIGGER_SECRET"], r.Header.Get("X-Tunr-Trigger-Signature"), b, time.Now()) {
		t.Fatalf("delivered %s %v %s", r.URL.Path, r.Header, b)
	}
	// Other hosts pass through untouched.
	req, _ := http.NewRequest("GET", e.srv.URL+"/anything", nil)
	req.Host = "someapp.tunr.test"
	resp, _ := http.DefaultClient.Do(req)
	resp.Body.Close()
	if resp.StatusCode != 200 || len(*got) != 2 {
		t.Errorf("web app passthrough: %d, %d dispatched", resp.StatusCode, len(*got))
	}

	// Cron: fires once per minute, even across ticks (and relay instances).
	now := time.Now().UTC().Truncate(time.Minute).Add(10 * time.Second)
	e.s.cronTick(context.Background(), now)
	e.s.cronTick(context.Background(), now.Add(5*time.Second))
	time.Sleep(200 * time.Millisecond)
	crons := 0
	for _, r := range *got {
		if r.Header.Get("X-Tunr-Trigger") == "cron:nightly" {
			crons++
			if r.URL.Path != "/run" {
				t.Errorf("cron path %s", r.URL.Path)
			}
		}
	}
	if crons != 1 {
		t.Errorf("cron fired %d times in one minute", crons)
	}

	// A disabled agent's hostname stays guarded; nothing reaches the container.
	e.call(e.vendor, "PATCH", "/orgs/vendor-co/agents/"+e.agentID, map[string]any{"status": "disabled"}, 200, nil)
	e.s.refreshHosted(context.Background())
	before := len(*got)
	if c := send("/", "", nil); c != 503 {
		t.Errorf("request to a disabled agent: %d", c)
	}
	if c := send("/__tunr/hooks/new-invoice", SignWebhook(hook.Secret, time.Now(), body), body); c != 503 {
		t.Errorf("hook to a disabled agent: %d", c)
	}
	if len(*got) != before {
		t.Errorf("disabled agent received %d requests", len(*got)-before)
	}
}

// A relay that starts while Postgres is down still guards agent hostnames it
// saw before, from the disk mirror.
func TestHostedMirrorGuardsColdStart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hosted.json")
	writeHostedMirror(path, []string{"bot"})
	if got := readHostedMirror(path); len(got) != 1 || got[0] != "bot" {
		t.Fatalf("mirror = %v", got)
	}
	s := &Server{cfg: Config{HostedCache: path}, hosted: &hostedCache{byName: map[string]db.HostedApp{}}}
	s.db = brokenDB(t)
	app, ok := s.hostedApp(context.Background(), "bot")
	if !ok || app.Active {
		t.Fatalf("cold start with DB down: ok=%v active=%v, want a guarded inactive entry", ok, app.Active)
	}
}

// brokenDB points at a closed port: every query fails fast.
func brokenDB(t *testing.T) *db.DB {
	t.Helper()
	d, _ := db.New(context.Background(), "postgres://x@127.0.0.1:1/x?connect_timeout=1") // non-nil even when unreachable
	if d == nil {
		t.Fatal("db.New returned nil")
	}
	return d
}
