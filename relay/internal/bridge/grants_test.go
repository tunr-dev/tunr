package bridge

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/tunr-dev/tunr/relay/internal/auth"
	"github.com/tunr-dev/tunr/relay/internal/db"
	"github.com/tunr-dev/tunr/relay/internal/db/dbtest"
)

// p4 is a full bridge server (API + hub + access) with a customer (acme:
// owner, approver2, plain member), a linked vendor with an agent, a site with
// a tcp and a postgres resource announced by a fake connector.
type p4 struct {
	t                                *testing.T
	d                                *db.DB
	s                                *Server
	srv                              *httptest.Server
	owner, approver2, member, vendor string // human tokens
	agentTok, agentID                string
	siteID                           string
	conn                             *websocket.Conn
	connMsgs                         chan Message
}

func newP4(t *testing.T) *p4 {
	t.Helper()
	old := sweepInterval
	t.Cleanup(func() { sweepInterval = old })
	d := dbtest.New(t)
	j, _ := auth.NewJWTAuth("supersecret-32-characters-minimum!!", time.Hour)
	_, seed, _ := auth.GenerateGrantSigner("x")
	s, err := New(Config{Enabled: true, SigningKey: seed, Issuer: "test", PublicURL: "http://x", PortalURL: "https://portal.test"},
		Deps{DB: d, JWT: j})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	s.RegisterRoutes(mux)
	e := &p4{t: t, d: d, s: s, srv: httptest.NewServer(mux)}
	t.Cleanup(e.srv.Close)
	ctx := context.Background()
	login := func(email string) (string, string) {
		uid := dbtest.User(t, d, email)
		tok, _ := j.Issue(uid, email, "free")
		return tok, uid
	}
	var ownerID string
	e.owner, ownerID = login("owner@acme.test")
	e.approver2, _ = login("second@acme.test")
	e.member, _ = login("member@acme.test")
	e.vendor, _ = login("dev@vendor.test")

	cust, _ := d.CreateOrg(ctx, ownerID, "Acme", "acme", "customer")
	_, _ = d.AddOrgMember(ctx, cust.ID, "second@acme.test", "approver", true)
	_, _ = d.AddOrgMember(ctx, cust.ID, "member@acme.test", "member", true)
	e.call(e.vendor, "POST", "/orgs", map[string]string{"name": "V", "slug": "vendor-co", "kind": "vendor"}, 201, nil)
	e.call(e.owner, "POST", "/orgs/acme/links", map[string]string{"vendor_slug": "vendor-co"}, 201, nil)
	var site siteSecret
	e.call(e.owner, "POST", "/orgs/acme/sites", map[string]string{"name": "hq-site"}, 201, &site)
	e.siteID = site.Site.ID
	var ag db.Agent
	e.call(e.vendor, "POST", "/orgs/vendor-co/agents", map[string]string{"name": "bot"}, 201, &ag)
	e.agentID = ag.ID
	var tok struct{ Token string }
	e.call(e.vendor, "POST", "/orgs/vendor-co/agents/"+ag.ID+"/tokens", nil, 201, &tok)
	e.agentTok = tok.Token

	// Fake connector: announces resources, records messages.
	c, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(e.srv.URL, "http")+"/bridge/connect",
		http.Header{"Authorization": {"Bearer " + site.Token}})
	if err != nil {
		t.Fatal(err)
	}
	_ = c.WriteJSON(msg(MsgHello, HelloData{Resources: []ResourceDecl{{"nas", "tcp"}, {"erp-db", "postgres"}}}))
	var w Message
	_ = c.ReadJSON(&w)
	e.conn, e.connMsgs = c, make(chan Message, 64)
	go func() {
		for {
			var m Message
			if c.ReadJSON(&m) != nil {
				return
			}
			e.connMsgs <- m
		}
	}()
	t.Cleanup(func() { c.Close() })
	return e
}

func (e *p4) call(tok, method, path string, body any, want int, out any) {
	e.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, e.srv.URL+"/api/v1/bridge"+path, rd)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != want {
		e.t.Fatalf("%s %s = %d (%s), want %d", method, path, resp.StatusCode, raw, want)
	}
	if out != nil {
		_ = json.Unmarshal(raw, out)
	}
}

func (e *p4) request(mode, resource, ttl string, want int) *db.GrantView {
	e.t.Helper()
	var g db.GrantView
	body := map[string]any{"resource": resource, "ttl": ttl, "reason": "October reconciliation"}
	if mode != "" {
		body["mode"] = mode
	}
	e.call(e.agentTok, "POST", "/agent/grants", body, want, &g)
	return &g
}

func TestGrantPolicy(t *testing.T) {
	e := newP4(t)
	var errBody struct{ Error string }
	cases := []struct {
		body map[string]any
		code string
		want int
	}{
		{map[string]any{"resource": "acme/erp-db", "mode": "rw", "ttl": "1h", "reason": "x"}, "mode_not_allowed", 400},
		{map[string]any{"resource": "acme/nas", "mode": "ro", "ttl": "1h", "reason": "x"}, "mode_not_enforceable", 400},
		{map[string]any{"resource": "acme/erp-db", "ttl": "10h", "reason": "x"}, "invalid_ttl", 400},
		{map[string]any{"resource": "acme/erp-db", "ttl": "30s", "reason": "x"}, "invalid_ttl", 400},
		{map[string]any{"resource": "acme/erp-db", "ttl": "1h", "reason": " "}, "invalid_reason", 400},
		{map[string]any{"resource": "other/erp-db", "ttl": "1h", "reason": "x"}, "unknown_resource", 404},
		{map[string]any{"resource": "acme/erp-db", "ttl": "1h", "reason": "x", "agent_id": "agt_someone_else"}, "forbidden", 403},
	}
	for _, c := range cases {
		e.call(e.agentTok, "POST", "/agent/grants", c.body, c.want, &errBody)
		if errBody.Error != c.code {
			t.Errorf("%v: error %q, want %q", c.body, errBody.Error, c.code)
		}
	}
	// Limits may narrow, never widen.
	res, _ := e.d.ListResources(context.Background(), mustOrg(t, e.d, "acme"))
	for _, r := range res {
		if r.Name == "erp-db" {
			e.call(e.owner, "PATCH", "/orgs/acme/resources/"+r.ID, map[string]any{"limits": map[string]int{"max_rows": 1000}}, 200, nil)
		}
	}
	e.call(e.agentTok, "POST", "/agent/grants", map[string]any{"resource": "acme/erp-db", "ttl": "1h", "reason": "x",
		"limits": map[string]int{"max_rows": 5000}}, 400, &errBody)
	if errBody.Error != "invalid_limits" {
		t.Errorf("widening limits: %q", errBody.Error)
	}
	g := e.request("", "acme/erp-db", "1h", 201)
	if g.Status != "pending" || g.Mode != "ro" || g.Address != "acme/erp-db" {
		t.Errorf("default-mode request = %+v", g)
	}
	// A human token is not an agent token.
	e.call(e.vendor, "GET", "/agent/catalog", nil, 401, nil)
}

func mustOrg(t *testing.T, d *db.DB, slug string) string {
	o, err := d.GetOrgBySlug(context.Background(), slug)
	if err != nil {
		t.Fatal(err)
	}
	return o.ID
}

func TestGrantApprovalFlows(t *testing.T) {
	e := newP4(t)

	// ro: one approver. Roles: plain member 403, vendor and outsiders 404.
	g := e.request("", "acme/erp-db", "2h", 201)
	e.call(e.member, "POST", "/orgs/acme/grants/"+g.ID+"/approve", nil, 403, nil)
	e.call(e.vendor, "POST", "/orgs/vendor-co/grants/"+g.ID+"/approve", nil, 404, nil)
	var ok db.GrantView
	e.call(e.approver2, "POST", "/orgs/acme/grants/"+g.ID+"/approve", nil, 200, &ok)
	if ok.Status != "approved" || ok.ExpiresAt == nil || time.Until(*ok.ExpiresAt) < 119*time.Minute {
		t.Fatalf("approved ro = %+v", ok)
	}
	e.call(e.owner, "POST", "/orgs/acme/grants/"+g.ID+"/approve", nil, 409, nil) // already approved
	e.call(e.owner, "POST", "/orgs/acme/grants/"+g.ID+"/deny", nil, 409, nil)

	var cat struct{ Resources []CatalogItem }
	e.call(e.agentTok, "GET", "/agent/catalog", nil, 200, &cat)
	for _, it := range cat.Resources {
		if it.Address == "acme/erp-db" && it.GrantStatus != "approved" {
			t.Errorf("catalog grant status = %q", it.GrantStatus)
		}
	}

	// rw: two different approvers.
	rw := e.request("rw", "acme/nas", "1h", 201)
	var first db.GrantView
	e.call(e.owner, "POST", "/orgs/acme/grants/"+rw.ID+"/approve", nil, 200, &first)
	if first.Status != "pending" || !first.FirstApproved {
		t.Fatalf("after first rw approval = %+v", first)
	}
	var errBody struct{ Error string }
	e.call(e.owner, "POST", "/orgs/acme/grants/"+rw.ID+"/approve", nil, 409, &errBody)
	if errBody.Error != "same_approver" {
		t.Errorf("same approver twice: %q", errBody.Error)
	}
	var second db.GrantView
	e.call(e.approver2, "POST", "/orgs/acme/grants/"+rw.ID+"/approve", nil, 200, &second)
	if second.Status != "approved" {
		t.Fatalf("after second rw approval = %+v", second)
	}

	// deny
	d := e.request("", "acme/erp-db", "1h", 201)
	var denied db.GrantView
	e.call(e.owner, "POST", "/orgs/acme/grants/"+d.ID+"/deny", nil, 200, &denied)
	if denied.Status != "denied" {
		t.Errorf("deny = %+v", denied)
	}

	// The agent sees its own grants only.
	var mine struct{ Grants []db.GrantView }
	e.call(e.agentTok, "GET", "/agent/grants", nil, 200, &mine)
	if len(mine.Grants) != 3 {
		t.Errorf("agent grants = %d", len(mine.Grants))
	}

	// Audit trail on the customer side contains the decisions.
	var aud struct{ Events []db.OrgAuditEvent }
	e.call(e.owner, "GET", "/orgs/acme/audit?action=bridge.grant_", nil, 200, &aud)
	seen := map[string]int{}
	for _, ev := range aud.Events {
		seen[ev.Action]++
	}
	if seen["bridge.grant_requested"] != 3 || seen["bridge.grant_approved"] != 2 ||
		seen["bridge.grant_first_approval"] != 1 || seen["bridge.grant_denied"] != 1 {
		t.Errorf("audit actions = %v", seen)
	}
}

func TestGrantAutoApprove(t *testing.T) {
	e := newP4(t)
	acme := mustOrg(t, e.d, "acme")
	vend := mustOrg(t, e.d, "vendor-co")
	res, _ := e.d.ListResources(context.Background(), acme)
	for _, r := range res {
		if r.Name == "erp-db" {
			e.call(e.owner, "PATCH", "/orgs/acme/resources/"+r.ID, map[string]any{
				"auto_approve": map[string]string{"vendor_org_id": vend, "mode": "ro", "max_ttl": "2h"}}, 200, nil)
		}
	}
	if g := e.request("", "acme/erp-db", "1h", 201); g.Status != "approved" {
		t.Errorf("within auto-approve policy: %s", g.Status)
	}
	if g := e.request("", "acme/erp-db", "3h", 201); g.Status != "pending" {
		t.Errorf("beyond auto-approve max_ttl: %s", g.Status)
	}
	if g := e.request("rw", "acme/nas", "1h", 201); g.Status != "pending" {
		t.Errorf("rw must never auto-approve: %s", g.Status)
	}
	var aud struct{ Events []db.OrgAuditEvent }
	e.call(e.owner, "GET", "/orgs/acme/audit?action=bridge.grant_auto_approved", nil, 200, &aud)
	if len(aud.Events) != 1 {
		t.Errorf("auto-approve audit events = %d", len(aud.Events))
	}
}

// openStream opens an agent stream on nas and returns the agent socket.
func (e *p4) openStream() *websocket.Conn {
	e.t.Helper()
	a, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(e.srv.URL, "http")+"/bridge/access?resource=acme/nas",
		http.Header{"Authorization": {"Bearer " + e.agentTok}})
	if err != nil {
		e.t.Fatalf("open stream: %v", err)
	}
	_ = a.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, _, err := a.ReadMessage(); err != nil { // access_ready
		e.t.Fatal(err)
	}
	return a
}

func waitClosed(t *testing.T, a *websocket.Conn, reason string, within time.Duration) {
	t.Helper()
	start := time.Now()
	_ = a.SetReadDeadline(time.Now().Add(within))
	for {
		_, b, err := a.ReadMessage()
		if err != nil {
			t.Fatalf("stream not closed with %q within %s: %v", reason, within, err)
		}
		if strings.Contains(string(b), MsgAccessClose) {
			if !strings.Contains(string(b), reason) {
				t.Fatalf("closed with %s, want %s", b, reason)
			}
			t.Logf("closed (%s) in %s", reason, time.Since(start))
			return
		}
	}
}

func (e *p4) expectKill(grantID string) {
	e.t.Helper()
	timeout := time.After(2 * time.Second)
	for {
		select {
		case m := <-e.connMsgs:
			if m.Type == MsgKill && strings.Contains(string(m.Data), grantID) {
				return
			}
		case <-timeout:
			e.t.Fatalf("connector never got bridge_kill for %s", grantID)
		}
	}
}

func (e *p4) approveRW() string {
	g := e.request("rw", "acme/nas", "1h", 201)
	e.call(e.owner, "POST", "/orgs/acme/grants/"+g.ID+"/approve", nil, 200, nil)
	e.call(e.approver2, "POST", "/orgs/acme/grants/"+g.ID+"/approve", nil, 200, nil)
	return g.ID
}

func TestRevokeCutsLiveStreamFast(t *testing.T) {
	e := newP4(t)
	gid := e.approveRW()
	a := e.openStream()
	defer a.Close()

	var g db.GrantView
	e.call(e.owner, "POST", "/orgs/acme/grants/"+gid+"/revoke", nil, 200, &g)
	waitClosed(t, a, "revoked", 2*time.Second) // invariant 8: ≤ 2 s
	e.expectKill(gid)
	if g.Status != "revoked" {
		t.Errorf("status %s", g.Status)
	}
	// And the agent can't come back on it.
	_, resp, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(e.srv.URL, "http")+"/bridge/access?resource=acme/nas",
		http.Header{"Authorization": {"Bearer " + e.agentTok}})
	if err == nil || resp.StatusCode != 403 {
		t.Fatalf("reconnect after revoke: %v %v", err, resp)
	}
	// Vendor may give up a grant too; a plain customer member may not.
	gid2 := e.approveRW()
	e.call(e.member, "POST", "/orgs/acme/grants/"+gid2+"/revoke", nil, 403, nil)
	e.call(e.vendor, "POST", "/orgs/vendor-co/grants/"+gid2+"/revoke", nil, 200, nil)
}

func TestKillSiteRevokesAndCuts(t *testing.T) {
	e := newP4(t)
	gid := e.approveRW()
	a := e.openStream()
	defer a.Close()
	var out struct {
		GrantsRevoked int `json:"grants_revoked"`
		StreamsClosed int `json:"streams_closed"`
	}
	e.call(e.member, "POST", "/orgs/acme/sites/"+e.siteID+"/kill", nil, 403, nil)
	e.call(e.owner, "POST", "/orgs/acme/sites/"+e.siteID+"/kill", nil, 200, &out)
	if out.GrantsRevoked != 1 || out.StreamsClosed != 1 {
		t.Errorf("kill = %+v", out)
	}
	waitClosed(t, a, "killed", 2*time.Second)
	e.expectKill("*")
	g, _ := e.d.GetGrantView(context.Background(), mustOrg(t, e.d, "acme"), gid)
	if g.Status != "revoked" {
		t.Errorf("grant after kill: %s", g.Status)
	}
}

func TestSweeperExpires(t *testing.T) {
	e := newP4(t)
	gid := e.approveRW()
	_ = e.d.QueryRowForTest(context.Background(),
		`UPDATE grants SET expires_at = now() - interval '1 second' WHERE id = $1 RETURNING id`, gid).Scan(new(string))
	e.s.sweepOnce(context.Background())
	g, _ := e.d.GetGrantView(context.Background(), mustOrg(t, e.d, "acme"), gid)
	if g.Status != "expired" {
		t.Fatalf("after sweep: %s", g.Status)
	}
	e.expectKill(gid)
	var aud struct{ Events []db.OrgAuditEvent }
	e.call(e.owner, "GET", "/orgs/acme/audit?action=bridge.grant_expired", nil, 200, &aud)
	if len(aud.Events) != 1 {
		t.Errorf("expired audit events = %d", len(aud.Events))
	}
}

func TestWebhookAndEmail(t *testing.T) {
	e := newP4(t)
	var mu sync.Mutex
	var hookBodies, sigs []string
	var mail map[string]any
	tlsSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		defer mu.Unlock()
		if r.URL.Path == "/emails" {
			_ = json.Unmarshal(b, &mail)
			if r.Header.Get("Authorization") != "Bearer re_test" {
				t.Errorf("resend auth header %q", r.Header.Get("Authorization"))
			}
		} else {
			hookBodies = append(hookBodies, string(b))
			sigs = append(sigs, r.Header.Get("X-Tunr-Signature"))
		}
	}))
	defer tlsSrv.Close()
	e.s.notify.HTTP = &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}
	e.s.notify.ResendKey, e.s.notify.ResendURL = "re_test", tlsSrv.URL+"/emails"

	e.call(e.owner, "POST", "/orgs/acme/webhooks", map[string]string{"url": "http://insecure.example"}, 400, nil)
	var created struct {
		Secret string
	}
	e.call(e.owner, "POST", "/orgs/acme/webhooks", map[string]string{"url": tlsSrv.URL + "/hook"}, 201, &created)
	var list struct{ Webhooks []map[string]any }
	e.call(e.owner, "GET", "/orgs/acme/webhooks", nil, 200, &list)
	if len(list.Webhooks) != 1 || list.Webhooks[0]["secret"] != nil {
		t.Fatalf("webhook list leaks secret or is wrong: %v", list)
	}

	g := e.request("", "acme/erp-db", "1h", 201)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n, m := len(hookBodies), mail
		mu.Unlock()
		if n >= 1 && m != nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(hookBodies) != 1 || !strings.Contains(hookBodies[0], `"grant.requested"`) || !strings.Contains(hookBodies[0], g.ID) {
		t.Fatalf("webhook bodies = %v", hookBodies)
	}
	// Signature verifies with the secret shown once.
	tsStr, _, _ := strings.Cut(strings.TrimPrefix(sigs[0], "t="), ",")
	ts, _ := strconv.ParseInt(tsStr, 10, 64)
	if want := SignWebhook(created.Secret, time.Unix(ts, 0), []byte(hookBodies[0])); sigs[0] != want {
		t.Errorf("signature %q, want %q", sigs[0], want)
	}
	to, _ := mail["to"].([]any)
	if len(to) != 2 || !strings.Contains(mail["subject"].(string), "acme/erp-db") ||
		!strings.Contains(mail["text"].(string), "https://portal.test/grants/"+g.ID) {
		t.Errorf("mail = %v", mail) // owner + approver (not the plain member)
	}
}

func TestAuditExportCSV(t *testing.T) {
	e := newP4(t)
	e.request("", "acme/erp-db", "1h", 201)
	req, _ := http.NewRequest("GET", e.srv.URL+"/api/v1/bridge/orgs/acme/audit/export?format=csv", nil)
	req.Header.Set("Authorization", "Bearer "+e.owner)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || !strings.HasPrefix(string(b), "id,created_at,action,user,detail") ||
		!strings.Contains(string(b), "bridge.grant_requested") {
		t.Fatalf("csv export %d: %s", resp.StatusCode, b)
	}
	if csvSafe("=cmd()") != "'=cmd()" || csvSafe("{}") != "{}" {
		t.Error("csvSafe")
	}
	e.call(e.member, "GET", "/orgs/acme/audit/export", nil, 403, nil)
}

func TestMetricsEndpoint(t *testing.T) {
	e := newP4(t)
	e.s.cfg.MetricsToken = "m-token"
	mux := http.NewServeMux()
	e.s.RegisterRoutes(mux)
	e.srv.Config.Handler = mux
	gid := e.approveRW()
	a := e.openStream()
	e.call(e.owner, "POST", "/orgs/acme/grants/"+gid+"/revoke", nil, 200, nil)
	a.Close()

	get := func(tok string) (int, string) {
		req, _ := http.NewRequest("GET", e.srv.URL+"/api/v1/bridge/metrics", nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	if code, _ := get("wrong"); code != 404 {
		t.Errorf("metrics without the token: %d", code)
	}
	code, body := get("m-token")
	for _, want := range []string{"tunr_bridge_sites_online 1", "tunr_bridge_active_sites_7d 1", `tunr_bridge_grants{status="revoked"} 1`,
		"tunr_bridge_kill_latency_seconds_count 1", "tunr_bridge_time_to_first_query_seconds_count", "tunr_bridge_grant_approval_seconds_count 1"} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics missing %q:\n%s", want, body)
		}
	}
	if code != 200 {
		t.Fatalf("metrics: %d", code)
	}
}
