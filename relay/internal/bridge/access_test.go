package bridge

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/tunr-dev/tunr/relay/internal/auth"
	"github.com/tunr-dev/tunr/relay/internal/db"
	"github.com/tunr-dev/tunr/relay/internal/db/dbtest"
)

// accessEnv: customer "acme" with site + tcp resource "echo", linked vendor
// with an agent and token, relay with hub + access mounted.
type accessEnv struct {
	t                 *testing.T
	d                 *db.DB
	srv               *httptest.Server
	jwt               *auth.JWTAuth
	signer            *auth.GrantSigner
	access            *Access
	cust, vend        *db.Org
	site              *db.Site
	siteToken         string
	res               *db.Resource
	agent             *db.Agent
	agentTok, tokenID string
}

func newAccessEnv(t *testing.T) *accessEnv {
	t.Helper()
	ctx := context.Background()
	d := dbtest.New(t)
	e := &accessEnv{t: t, d: d}
	e.jwt, _ = auth.NewJWTAuth("supersecret-32-characters-minimum!!", time.Hour)
	e.signer, _, _ = auth.GenerateGrantSigner("relay.test")
	cu, vu := dbtest.User(t, d, "c@acme.test"), dbtest.User(t, d, "v@vendor.test")
	var err error
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	e.cust, err = d.CreateOrg(ctx, cu, "Acme", "acme", "customer")
	must(err)
	e.vend, err = d.CreateOrg(ctx, vu, "Vendor", "vendor-co", "vendor")
	must(err)
	_, err = d.CreateLink(ctx, e.cust.ID, e.vend.ID, cu)
	must(err)
	var hash string
	e.siteToken, hash, _ = auth.GenerateSiteToken()
	e.site, err = d.CreateSite(ctx, e.cust.ID, "hq-site", hash)
	must(err)
	e.agent, err = d.CreateAgent(ctx, e.vend.ID, "bot")
	must(err)
	rec, err := d.CreateAgentToken(ctx, e.vend.ID, e.agent.ID, vu, time.Now().Add(time.Hour))
	must(err)
	e.tokenID = rec.ID
	e.agentTok, err = e.jwt.IssueAgent(e.vend.ID, e.agent.ID, rec.ID, rec.ExpiresAt)
	must(err)

	hub := NewConnectorHub(d, e.signer.KeyString())
	e.access = NewAccess(d, e.jwt, e.signer, hub)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /bridge/connect", hub.ServeConnect)
	mux.HandleFunc("GET /bridge/access", e.access.ServeAccess)
	mux.HandleFunc("GET /bridge/access/check", e.access.ServeCheck)
	e.srv = httptest.NewServer(mux)
	t.Cleanup(e.srv.Close)
	return e
}

func (e *accessEnv) wsURL(path string) string {
	return "ws" + strings.TrimPrefix(e.srv.URL, "http") + path
}

// connector attaches a fake connector that echoes stream bytes uppercased and
// records every message; it returns the conn and a channel of messages.
func (e *accessEnv) connector(token string) (*websocket.Conn, chan Message) {
	e.t.Helper()
	c, _, err := websocket.DefaultDialer.Dial(e.wsURL("/bridge/connect"), http.Header{"Authorization": {"Bearer " + token}})
	if err != nil {
		e.t.Fatalf("connector dial: %v", err)
	}
	_ = c.WriteJSON(msg(MsgHello, HelloData{ConnectorVersion: "test", Resources: []ResourceDecl{{Name: "echo", Kind: "tcp"}}}))
	var w Message
	if err := c.ReadJSON(&w); err != nil || w.Type != MsgWelcome {
		e.t.Fatalf("welcome: %v %s", err, w.Type)
	}
	var wd WelcomeData
	_ = json.Unmarshal(w.Data, &wd)
	if e.res == nil {
		e.res, _ = e.d.GetResource(context.Background(), e.cust.ID, wd.Resources[0].ID)
	}
	got := make(chan Message, 64)
	go func() {
		for {
			var m Message
			if err := c.ReadJSON(&m); err != nil {
				close(got)
				return
			}
			if m.Type == MsgData {
				var d DataPayload
				_ = json.Unmarshal(m.Data, &d)
				b, _ := base64.StdEncoding.DecodeString(d.PayloadB64)
				_ = c.WriteJSON(msg(MsgData, DataPayload{StreamID: d.StreamID,
					PayloadB64: base64.StdEncoding.EncodeToString([]byte(strings.ToUpper(string(b))))}))
			}
			got <- m
		}
	}()
	e.t.Cleanup(func() { c.Close() })
	return c, got
}

// approve inserts an approved grant directly (the approval API is P4).
func (e *accessEnv) approve(mode string, ttl time.Duration) string {
	e.t.Helper()
	g, err := e.d.CreateGrantRequest(context.Background(), e.vend.ID, e.agent.ID, e.res.ID, mode, "test", ttl)
	if err != nil {
		e.t.Fatal(err)
	}
	var id string
	if err := e.d.QueryRowForTest(context.Background(), `
		UPDATE grants SET status='approved', approved_at=now(), expires_at=now() + make_interval(secs => $2)
		WHERE id=$1 RETURNING id`, g.ID, ttl.Seconds()).Scan(&id); err != nil {
		e.t.Fatal(err)
	}
	return id
}

func (e *accessEnv) dialAgent(token, resource string) (*websocket.Conn, int, string) {
	e.t.Helper()
	hdr := http.Header{}
	if token != "" {
		hdr.Set("Authorization", "Bearer "+token)
	}
	c, resp, err := websocket.DefaultDialer.Dial(e.wsURL("/bridge/access?resource="+resource), hdr)
	if err != nil {
		if resp == nil {
			e.t.Fatalf("dial: %v", err)
		}
		b, _ := io.ReadAll(resp.Body)
		var body struct{ Error string }
		_ = json.Unmarshal(b, &body)
		return nil, resp.StatusCode, body.Error
	}
	return c, http.StatusSwitchingProtocols, ""
}

func next(t *testing.T, ch chan Message, typ string) Message {
	t.Helper()
	timeout := time.After(3 * time.Second)
	for {
		select {
		case m, ok := <-ch:
			if !ok {
				t.Fatalf("channel closed waiting for %s", typ)
			}
			if m.Type == typ {
				return m
			}
		case <-timeout:
			t.Fatalf("timed out waiting for %s", typ)
		}
	}
}

func readAgent(t *testing.T, c *websocket.Conn) (int, []byte) {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	typ, b, err := c.ReadMessage()
	if err != nil {
		t.Fatalf("agent read: %v", err)
	}
	return typ, b
}

func TestAccessRefusals(t *testing.T) {
	e := newAccessEnv(t)
	human, _ := e.jwt.Issue("u", "u@x", "free")

	cases := []struct {
		name, token, resource, code string
		status                      int
	}{
		{"no token", "", "acme/echo", "auth_required", 401},
		{"human token", human, "acme/echo", "invalid_token", 401},
		{"bad address", e.agentTok, "echo", "bad_resource", 400},
		{"unknown customer", e.agentTok, "other/echo", "unknown_resource", 404},
	}
	for _, c := range cases {
		if _, st, code := e.dialAgent(c.token, c.resource); st != c.status || code != c.code {
			t.Errorf("%s: %d %s, want %d %s", c.name, st, code, c.status, c.code)
		}
	}

	_, _ = e.connector(e.siteToken) // resource exists now
	check := func() (int, string) {
		req, _ := http.NewRequest("GET", e.srv.URL+"/bridge/access/check?resource=acme/echo", nil)
		req.Header.Set("Authorization", "Bearer "+e.agentTok)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var body struct{ Error, Mode string }
		_ = json.NewDecoder(resp.Body).Decode(&body)
		return resp.StatusCode, body.Error + body.Mode
	}
	if st, code := check(); st != 403 || code != "no_grant" {
		t.Errorf("check without grant: %d %s", st, code)
	}
	if _, st, code := e.dialAgent(e.agentTok, "acme/echo"); st != 403 || code != "no_grant" {
		t.Errorf("no grant: %d %s", st, code)
	}
	// Pending (not approved) grant is still no grant.
	if _, err := e.d.CreateGrantRequest(context.Background(), e.vend.ID, e.agent.ID, e.res.ID, "rw", "", time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, st, code := e.dialAgent(e.agentTok, "acme/echo"); st != 403 || code != "no_grant" {
		t.Errorf("pending grant: %d %s", st, code)
	}
	e.approve("rw", time.Hour)
	if st, mode := check(); st != 200 || mode != "rw" {
		t.Errorf("check with grant: %d %s", st, mode)
	}
	// Revoked token.
	_ = e.d.RevokeAgentToken(context.Background(), e.vend.ID, e.agent.ID, e.tokenID)
	if _, st, code := e.dialAgent(e.agentTok, "acme/echo"); st != 401 || code != "invalid_token" {
		t.Errorf("revoked token: %d %s", st, code)
	}
}

func TestAccessSiteOffline(t *testing.T) {
	e := newAccessEnv(t)
	c, _ := e.connector(e.siteToken)
	e.approve("rw", time.Hour)
	c.Close()
	waitFor := func(cond func() bool) {
		for i := 0; i < 100 && !cond(); i++ {
			time.Sleep(20 * time.Millisecond)
		}
	}
	waitFor(func() bool { _, st, _ := e.dialAgent(e.agentTok, "acme/echo"); return st == 503 })
	if _, st, code := e.dialAgent(e.agentTok, "acme/echo"); st != 503 || code != "site_offline" {
		t.Errorf("offline: %d %s", st, code)
	}
}

func TestAccessStreamRoundTrip(t *testing.T) {
	e := newAccessEnv(t)
	_, conn := e.connector(e.siteToken)
	grantID := e.approve("rw", time.Hour)

	a, st, code := e.dialAgent(e.agentTok, "acme/echo")
	if a == nil {
		t.Fatalf("dial: %d %s", st, code)
	}
	defer a.Close()

	// Relay → agent: access_ready.
	typ, b := readAgent(t, a)
	var ready Message
	_ = json.Unmarshal(b, &ready)
	var rd AccessReady
	_ = json.Unmarshal(ready.Data, &rd)
	if typ != websocket.TextMessage || ready.Type != MsgAccessReady || rd.GrantID != grantID || rd.Mode != "rw" {
		t.Fatalf("ready: %s", b)
	}

	// Relay → connector: bridge_open with a grant JWT bound to this stream.
	open := next(t, conn, MsgOpen)
	var od OpenData
	_ = json.Unmarshal(open.Data, &od)
	claims, err := auth.VerifyGrant(e.signer.PublicKey(), od.GrantJWT, e.site.ID)
	if err != nil {
		t.Fatalf("grant jwt: %v", err)
	}
	if od.StreamID != rd.StreamID || claims.ID != od.StreamID || claims.GrantID != grantID ||
		claims.Resource != e.res.ID || claims.Subject != e.agent.ID || claims.Mode != "rw" {
		t.Fatalf("open %+v claims %+v", od, claims)
	}

	// Bytes both ways.
	_ = a.WriteMessage(websocket.BinaryMessage, []byte("select 1"))
	if typ, b := readAgent(t, a); typ != websocket.BinaryMessage || string(b) != "SELECT 1" {
		t.Fatalf("echo: %d %q", typ, b)
	}

	// Agent closes → connector told, session row closed with byte counts.
	a.Close()
	if cl := next(t, conn, MsgClose); !strings.Contains(string(cl.Data), od.StreamID) {
		t.Fatalf("close: %s", cl.Data)
	}
	var up, down int64
	var reason string
	for i := 0; i < 100; i++ {
		err = e.d.QueryRowForTest(context.Background(),
			`SELECT bytes_up, bytes_down, close_reason FROM access_sessions WHERE id=$1 AND closed_at IS NOT NULL`, od.StreamID).
			Scan(&up, &down, &reason)
		if err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if up != 8 || down != 8 || reason != "client" {
		t.Errorf("session row: up=%d down=%d reason=%q err=%v", up, down, reason, err)
	}
	var n int
	for i := 0; i < 100 && n < 4; i++ {
		_ = e.d.QueryRowForTest(context.Background(),
			`SELECT count(*) FROM audit_log WHERE action LIKE 'bridge.session_%' AND detail->>'session_id' = $1`, od.StreamID).Scan(&n)
		time.Sleep(10 * time.Millisecond)
	}
	if n != 4 { // opened + closed, for customer and vendor
		t.Errorf("session audit events = %d, want 4", n)
	}
}

func TestAccessConnectorCloseKillAndExpiry(t *testing.T) {
	e := newAccessEnv(t)
	cc, conn := e.connector(e.siteToken)
	grantID := e.approve("rw", time.Hour)

	expectClose := func(a *websocket.Conn, reason string) {
		t.Helper()
		for {
			_ = a.SetReadDeadline(time.Now().Add(3 * time.Second))
			typ, b, err := a.ReadMessage()
			if err != nil {
				t.Fatalf("waiting for close %q: %v", reason, err)
			}
			if typ == websocket.TextMessage && strings.Contains(string(b), MsgAccessClose) {
				if !strings.Contains(string(b), reason) {
					t.Fatalf("close reason: %s, want %s", b, reason)
				}
				return
			}
		}
	}

	// Connector-initiated close.
	a1, _, _ := e.dialAgent(e.agentTok, "acme/echo")
	open := next(t, conn, MsgOpen)
	var od OpenData
	_ = json.Unmarshal(open.Data, &od)
	_ = cc.WriteJSON(msg(MsgClose, CloseData{StreamID: od.StreamID, Reason: "invalid_grant"}))
	expectClose(a1, "invalid_grant")

	// Grant revocation path (P4 calls KillGrant).
	a2, _, _ := e.dialAgent(e.agentTok, "acme/echo")
	next(t, conn, MsgOpen)
	time.Sleep(50 * time.Millisecond)
	if n := e.access.KillGrant(grantID, "revoked"); n != 1 {
		t.Fatalf("KillGrant closed %d streams", n)
	}
	expectClose(a2, "revoked")

	// Expiry: a grant ending in 1s closes its stream on time.
	_ = e.d.QueryRowForTest(context.Background(),
		`UPDATE grants SET expires_at = now() + interval '1 second' WHERE id=$1 RETURNING id`, grantID).Scan(new(string))
	a3, _, _ := e.dialAgent(e.agentTok, "acme/echo")
	start := time.Now()
	expectClose(a3, "expired")
	if time.Since(start) > 2500*time.Millisecond {
		t.Errorf("expiry close took %s", time.Since(start))
	}
}

// A connector can only speak for its own site's streams.
func TestAccessCrossSiteInjection(t *testing.T) {
	e := newAccessEnv(t)
	_, conn := e.connector(e.siteToken)
	e.approve("rw", time.Hour)

	tok2, hash2, _ := auth.GenerateSiteToken()
	if _, err := e.d.CreateSite(context.Background(), e.cust.ID, "evil-site", hash2); err != nil {
		t.Fatal(err)
	}
	evil, _, err := websocket.DefaultDialer.Dial(e.wsURL("/bridge/connect"), http.Header{"Authorization": {"Bearer " + tok2}})
	if err != nil {
		t.Fatal(err)
	}
	defer evil.Close()
	_ = evil.WriteJSON(msg(MsgHello, HelloData{}))
	var w Message
	_ = evil.ReadJSON(&w)

	a, _, _ := e.dialAgent(e.agentTok, "acme/echo")
	defer a.Close()
	readAgent(t, a) // access_ready
	open := next(t, conn, MsgOpen)
	var od OpenData
	_ = json.Unmarshal(open.Data, &od)

	_ = evil.WriteJSON(msg(MsgData, DataPayload{StreamID: od.StreamID, PayloadB64: base64.StdEncoding.EncodeToString([]byte("INJECTED"))}))
	_ = evil.WriteJSON(msg(MsgClose, CloseData{StreamID: od.StreamID, Reason: "evil"}))
	_ = a.WriteMessage(websocket.BinaryMessage, []byte("real"))
	if _, b := readAgent(t, a); string(b) != "REAL" {
		t.Fatalf("agent got %q — injected data reached the stream", b)
	}
}

func TestAccessConnectorDropClosesStreams(t *testing.T) {
	e := newAccessEnv(t)
	cc, conn := e.connector(e.siteToken)
	e.approve("rw", time.Hour)
	a, _, _ := e.dialAgent(e.agentTok, "acme/echo")
	defer a.Close()
	readAgent(t, a)
	next(t, conn, MsgOpen)
	cc.Close()
	for {
		_ = a.SetReadDeadline(time.Now().Add(3 * time.Second))
		_, b, err := a.ReadMessage()
		if err != nil {
			t.Fatalf("agent stream not closed after connector dropped: %v", err)
		}
		if strings.Contains(string(b), "site_offline") {
			return
		}
	}
}

func TestEffectiveLimits(t *testing.T) {
	got := effectiveLimits(json.RawMessage(`{"max_rows":10000,"qps":5}`), json.RawMessage(`{"max_rows":500,"max_bytes":1000}`))
	if got == nil || got.MaxRows != 500 || got.QPS != 5 || got.MaxBytes != 1000 {
		t.Errorf("effectiveLimits = %+v", got)
	}
	if effectiveLimits(json.RawMessage(`{}`), nil) != nil {
		t.Error("empty limits should be nil")
	}
}

func TestAccessQuerySummariesPersisted(t *testing.T) {
	e := newAccessEnv(t)
	cc, conn := e.connector(e.siteToken)
	e.approve("rw", time.Hour)
	a, _, _ := e.dialAgent(e.agentTok, "acme/echo")
	defer a.Close()
	readAgent(t, a)
	open := next(t, conn, MsgOpen)
	var od OpenData
	_ = json.Unmarshal(open.Data, &od)

	send := func(ad AuditData) {
		ad.StreamID = od.StreamID
		_ = cc.WriteJSON(msg(MsgAudit, ad))
	}
	send(AuditData{Command: "SELECT", Tables: []string{"fatura"}, RowCount: 42, DurationMS: 3, QueryText: "select * from fatura"})
	send(AuditData{Command: "DELETE", Tables: []string{"fatura"}, Blocked: true, QueryText: "delete from fatura"})

	type row struct {
		cmd, text string
		rows      int
		blocked   bool
	}
	read := func(want int) []row {
		var out []row
		for i := 0; i < 100; i++ {
			out = nil
			rs, err := e.d.ListSessionQueries(context.Background(), e.cust.ID, od.StreamID, true)
			if err == nil {
				for _, q := range rs {
					r := row{cmd: q.Command, text: q.QueryText, blocked: q.Blocked}
					if q.RowCount != nil {
						r.rows = *q.RowCount
					}
					out = append(out, r)
				}
			}
			if len(out) >= want {
				return out
			}
			time.Sleep(20 * time.Millisecond)
		}
		return out
	}
	got := read(2)
	if len(got) != 2 || got[0].cmd != "SELECT" || got[0].rows != 42 || got[1].cmd != "blocked:DELETE" || !got[1].blocked {
		t.Fatalf("summaries = %+v", got)
	}
	if got[0].text != "" || got[1].text != "" {
		t.Errorf("query text stored without the customer opting in: %+v", got)
	}

	// Opt in → text is kept.
	_ = e.d.QueryRowForTest(context.Background(),
		`UPDATE orgs SET settings = '{"log_query_text": true}' WHERE id = $1 RETURNING id`, e.cust.ID).Scan(new(string))
	send(AuditData{Command: "SELECT", QueryText: "select 1"})
	got = read(3)
	if len(got) != 3 || got[2].text != "select 1" {
		t.Errorf("opted-in text: %+v", got)
	}

	var blocked int
	for i := 0; i < 50 && blocked < 2; i++ {
		_ = e.d.QueryRowForTest(context.Background(),
			`SELECT count(*) FROM audit_log WHERE action = 'bridge.query_blocked' AND detail->>'session_id' = $1`, od.StreamID).Scan(&blocked)
		time.Sleep(20 * time.Millisecond)
	}
	if blocked != 2 {
		t.Errorf("query_blocked audit events = %d, want 2 (customer + vendor)", blocked)
	}
	var qc int
	_ = e.d.QueryRowForTest(context.Background(), `SELECT query_count FROM access_sessions WHERE id = $1`, od.StreamID).Scan(&qc)
	if qc != 3 {
		t.Errorf("session query_count = %d", qc)
	}
}

// Summaries that arrive after the agent hung up are still filed — but only
// when the sending connector is the session's own site.
func TestAccessLateSummary(t *testing.T) {
	e := newAccessEnv(t)
	cc, conn := e.connector(e.siteToken)
	e.approve("rw", time.Hour)
	a, _, _ := e.dialAgent(e.agentTok, "acme/echo")
	readAgent(t, a)
	open := next(t, conn, MsgOpen)
	var od OpenData
	_ = json.Unmarshal(open.Data, &od)
	a.Close()
	next(t, conn, MsgClose) // relay has dropped the stream

	// A different site's connector tries to file one for this session first.
	tok2, hash2, _ := auth.GenerateSiteToken()
	_, _ = e.d.CreateSite(context.Background(), e.cust.ID, "other-site", hash2)
	evil, _, err := websocket.DefaultDialer.Dial(e.wsURL("/bridge/connect"), http.Header{"Authorization": {"Bearer " + tok2}})
	if err != nil {
		t.Fatal(err)
	}
	defer evil.Close()
	_ = evil.WriteJSON(msg(MsgHello, HelloData{}))
	var w Message
	_ = evil.ReadJSON(&w)
	_ = evil.WriteJSON(msg(MsgAudit, AuditData{StreamID: od.StreamID, Command: "FORGED"}))

	_ = cc.WriteJSON(msg(MsgAudit, AuditData{StreamID: od.StreamID, Command: "DELETE", Blocked: true}))
	var cmds []string
	for i := 0; i < 100 && len(cmds) == 0; i++ {
		qs, _ := e.d.ListSessionQueries(context.Background(), e.cust.ID, od.StreamID, false)
		cmds = nil
		for _, q := range qs {
			cmds = append(cmds, q.Command)
		}
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)
	qs, _ := e.d.ListSessionQueries(context.Background(), e.cust.ID, od.StreamID, false)
	if len(qs) != 1 || qs[0].Command != "blocked:DELETE" {
		t.Fatalf("late summaries = %+v (forged one must be dropped)", qs)
	}
}

// E2E is used only when the agent asks AND the connector announced a cert.
func TestAccessE2ENegotiation(t *testing.T) {
	e := newAccessEnv(t)
	fp := strings.Repeat("ab", 32)
	c, _, err := websocket.DefaultDialer.Dial(e.wsURL("/bridge/connect"), http.Header{"Authorization": {"Bearer " + e.siteToken}})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.WriteJSON(msg(MsgHello, HelloData{Resources: []ResourceDecl{{Name: "echo", Kind: "tcp"}}, TLSCertSHA256: fp}))
	var w Message
	_ = c.ReadJSON(&w)
	var wd WelcomeData
	_ = json.Unmarshal(w.Data, &wd)
	e.res, _ = e.d.GetResource(context.Background(), e.cust.ID, wd.Resources[0].ID)
	e.approve("rw", time.Hour)
	if s, _ := e.d.GetSite(context.Background(), e.cust.ID, e.site.ID); s.TLSFingerprint != fp {
		t.Errorf("site fingerprint not stored: %q", s.TLSFingerprint)
	}

	dial := func(e2e bool) (AccessReady, OpenData) {
		hdr := http.Header{"Authorization": {"Bearer " + e.agentTok}}
		if e2e {
			hdr.Set("X-Tunr-E2E", "1")
		}
		a, _, err := websocket.DefaultDialer.Dial(e.wsURL("/bridge/access?resource=acme/echo"), hdr)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { a.Close() })
		_, b := readAgent(t, a)
		var m Message
		_ = json.Unmarshal(b, &m)
		var rd AccessReady
		_ = json.Unmarshal(m.Data, &rd)
		var om Message
		for om.Type != MsgOpen {
			_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
			if err := c.ReadJSON(&om); err != nil {
				t.Fatal(err)
			}
		}
		var od OpenData
		_ = json.Unmarshal(om.Data, &od)
		return rd, od
	}
	rd, od := dial(true)
	if !rd.E2E || rd.SiteCertSHA256 != fp || !od.E2E {
		t.Errorf("e2e requested: ready=%+v open.e2e=%v", rd, od.E2E)
	}
	rd, od = dial(false)
	if rd.E2E || rd.SiteCertSHA256 != "" || od.E2E {
		t.Errorf("e2e not requested: ready=%+v open.e2e=%v", rd, od.E2E)
	}
}

func TestConnectRejectsBadTLSFingerprint(t *testing.T) {
	e := newHubEnv(t)
	c, _, err := e.dial(e.token, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.WriteJSON(msg(MsgHello, HelloData{TLSCertSHA256: "not-hex"}))
	if m := e.read(c); m.Type != MsgError || !strings.Contains(string(m.Data), "bad_hello") {
		t.Fatalf("got %s %s", m.Type, m.Data)
	}
}
