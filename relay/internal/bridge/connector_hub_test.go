package bridge

import (
	"context"
	"encoding/json"
	"errors"
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

type hubEnv struct {
	t     *testing.T
	d     *db.DB
	hub   *ConnectorHub
	srv   *httptest.Server
	org   *db.Org
	site  *db.Site
	token string
}

func newHubEnv(t *testing.T) *hubEnv {
	t.Helper()
	d := dbtest.New(t)
	ctx := context.Background()
	uid := dbtest.User(t, d, "it@acme.test")
	org, err := d.CreateOrg(ctx, uid, "Acme", "acme", "customer")
	if err != nil {
		t.Fatal(err)
	}
	tok, hash, _ := auth.GenerateSiteToken()
	site, err := d.CreateSite(ctx, org.ID, "istanbul-ofis", hash)
	if err != nil {
		t.Fatal(err)
	}
	hub := NewConnectorHub(d, "ed25519:test-key")
	mux := http.NewServeMux()
	mux.HandleFunc("GET /bridge/connect", hub.ServeConnect)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &hubEnv{t: t, d: d, hub: hub, srv: srv, org: org, site: site, token: tok}
}

func (e *hubEnv) dial(token string, hdr http.Header) (*websocket.Conn, *http.Response, error) {
	if hdr == nil {
		hdr = http.Header{}
	}
	if token != "" {
		hdr.Set("Authorization", "Bearer "+token)
	}
	return websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(e.srv.URL, "http")+"/bridge/connect", hdr)
}

// connect dials, sends hello and returns the welcome.
func (e *hubEnv) connect(resources ...ResourceDecl) (*websocket.Conn, WelcomeData) {
	e.t.Helper()
	c, _, err := e.dial(e.token, nil)
	if err != nil {
		e.t.Fatalf("dial: %v", err)
	}
	if err := c.WriteJSON(msg(MsgHello, HelloData{ConnectorVersion: "0.8.0-test", Resources: resources})); err != nil {
		e.t.Fatal(err)
	}
	m := e.read(c)
	if m.Type != MsgWelcome {
		e.t.Fatalf("got %s %s, want welcome", m.Type, m.Data)
	}
	var w WelcomeData
	_ = json.Unmarshal(m.Data, &w)
	return c, w
}

func (e *hubEnv) read(c *websocket.Conn) Message {
	e.t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	var m Message
	if err := c.ReadJSON(&m); err != nil {
		e.t.Fatalf("read: %v", err)
	}
	return m
}

func (e *hubEnv) siteStatus() string {
	s, err := e.d.GetSite(context.Background(), e.org.ID, e.site.ID)
	if err != nil {
		e.t.Fatal(err)
	}
	return s.Status
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for i := 0; i < 100; i++ {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestConnectAuthBeforeUpgrade(t *testing.T) {
	e := newHubEnv(t)
	for name, tok := range map[string]string{
		"no token":    "",
		"wrong token": auth.SiteTokenPrefix + "nope",
		"human jwt":   "eyJhbGciOiJIUzI1NiJ9.e30.x",
	} {
		_, resp, err := e.dial(tok, nil)
		if err == nil || resp == nil || resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s: err=%v status=%v, want 401 without upgrade", name, err, resp)
		}
	}
	// A browser page (Origin set) can't open a connector socket even with a token.
	_, resp, err := e.dial(e.token, http.Header{"Origin": {"https://evil.example"}})
	if err == nil || resp == nil || resp.StatusCode != http.StatusForbidden {
		t.Errorf("origin: err=%v resp=%v, want 403", err, resp)
	}
	if e.siteStatus() != "offline" {
		t.Error("rejected attempts changed site status")
	}
}

func TestConnectHelloSyncsResources(t *testing.T) {
	e := newHubEnv(t)
	c, w := e.connect(ResourceDecl{"erp-db", "postgres"}, ResourceDecl{"nas", "tcp"})
	if w.SiteID != e.site.ID || w.RelayKey != "ed25519:test-key" || len(w.Resources) != 2 {
		t.Fatalf("welcome = %+v", w)
	}
	if !e.hub.Online(e.site.ID) || e.siteStatus() != "online" {
		t.Fatal("site not online after welcome")
	}
	c.Close()
	eventually(t, "offline after disconnect", func() bool { return !e.hub.Online(e.site.ID) && e.siteStatus() == "offline" })

	// Reconnect announcing only erp-db: nas goes offline, erp-db keeps its ID.
	c2, w2 := e.connect(ResourceDecl{"erp-db", "postgres"})
	defer c2.Close()
	if len(w2.Resources) != 1 || w2.Resources[0].ID != w.Resources[0].ID {
		t.Fatalf("welcome after reconnect = %+v (first %+v)", w2, w)
	}
	res, _ := e.d.ListResources(context.Background(), e.org.ID)
	status := map[string]string{}
	for _, r := range res {
		status[r.Name] = r.Status
	}
	if status["erp-db"] != "active" || status["nas"] != "offline" {
		t.Errorf("resource status = %v", status)
	}
}

func TestConnectRejectsBadHello(t *testing.T) {
	e := newHubEnv(t)
	for name, hello := range map[string]HelloData{
		"bad name":  {Resources: []ResourceDecl{{"../etc", "tcp"}}},
		"bad kind":  {Resources: []ResourceDecl{{"erp-db", "ssh"}}},
		"duplicate": {Resources: []ResourceDecl{{"erp-db", "tcp"}, {"erp-db", "tcp"}}},
		"long ver":  {ConnectorVersion: strings.Repeat("x", 100)},
	} {
		c, _, err := e.dial(e.token, nil)
		if err != nil {
			t.Fatal(err)
		}
		_ = c.WriteJSON(msg(MsgHello, hello))
		m := e.read(c)
		if m.Type != MsgError || !strings.Contains(string(m.Data), "bad_hello") {
			t.Errorf("%s: got %s %s", name, m.Type, m.Data)
		}
		c.Close()
	}
	if e.siteStatus() != "offline" {
		t.Error("bad hello marked site online")
	}
}

func TestConnectResourceConflictAcrossSites(t *testing.T) {
	e := newHubEnv(t)
	c, _ := e.connect(ResourceDecl{"erp-db", "postgres"})
	defer c.Close()

	tok2, hash2, _ := auth.GenerateSiteToken()
	if _, err := e.d.CreateSite(context.Background(), e.org.ID, "ankara-ofis", hash2); err != nil {
		t.Fatal(err)
	}
	c2, _, err := e.dial(tok2, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	_ = c2.WriteJSON(msg(MsgHello, HelloData{Resources: []ResourceDecl{{"erp-db", "tcp"}}}))
	if m := e.read(c2); m.Type != MsgError || !strings.Contains(string(m.Data), "resource_conflict") {
		t.Fatalf("got %s %s, want resource_conflict", m.Type, m.Data)
	}
	r, _ := e.d.ListResources(context.Background(), e.org.ID)
	if len(r) != 1 || r[0].SiteID != e.site.ID || r[0].Kind != "postgres" {
		t.Errorf("resource hijacked: %+v", r)
	}
}

func TestConnectNewestWinsAndDisconnect(t *testing.T) {
	e := newHubEnv(t)
	c1, _ := e.connect()
	c2, _ := e.connect()
	defer c2.Close()

	if m := e.read(c1); m.Type != MsgError || !strings.Contains(string(m.Data), "replaced") {
		t.Fatalf("old session got %s %s, want replaced error", m.Type, m.Data)
	}
	// The replaced session's teardown must not mark the site offline.
	time.Sleep(100 * time.Millisecond)
	if !e.hub.Online(e.site.ID) || e.siteStatus() != "online" {
		t.Fatal("site went offline when the old session was replaced")
	}

	if err := e.hub.Send(e.site.ID, msg(MsgKill, KillData{GrantID: "*"})); err != nil {
		t.Fatal(err)
	}
	if m := e.read(c2); m.Type != MsgKill {
		t.Fatalf("got %s, want kill", m.Type)
	}

	e.hub.Disconnect(e.site.ID, "site token rotated")
	if m := e.read(c2); m.Type != MsgError || !strings.Contains(string(m.Data), "rotated") {
		t.Fatalf("got %s %s, want disconnect error", m.Type, m.Data)
	}
	eventually(t, "offline", func() bool { return e.siteStatus() == "offline" })
	if err := e.hub.Send(e.site.ID, Message{Type: MsgPing}); err != ErrSiteOffline {
		t.Errorf("Send after disconnect: %v", err)
	}
}

func TestConnectPingAndSink(t *testing.T) {
	old := pingInterval
	pingInterval = 50 * time.Millisecond
	defer func() { pingInterval = old }()

	e := newHubEnv(t)
	got := make(chan Message, 1)
	e.hub.SetSink(sinkFunc(func(siteID string, m Message) {
		if siteID == e.site.ID {
			got <- m
		}
	}))
	c, _ := e.connect()
	defer c.Close()
	if m := e.read(c); m.Type != MsgPing {
		t.Fatalf("got %s, want ping", m.Type)
	}
	_ = c.WriteJSON(Message{Type: MsgPong})
	_ = c.WriteJSON(msg(MsgClose, CloseData{StreamID: "s1", Reason: "client"}))
	select {
	case m := <-got:
		if m.Type != MsgClose {
			t.Fatalf("sink got %s", m.Type)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("sink not called")
	}
}

type sinkFunc func(string, Message)

func (f sinkFunc) FromConnector(siteID string, m Message) { f(siteID, m) }
func (f sinkFunc) SiteGone(string)                        {}

// One busy stream fills only its share of the site's queue and is slowed or
// closed itself; the site stays connected and control messages still fit.
func TestSendDataBackpressureKeepsSite(t *testing.T) {
	h := NewConnectorHub(nil, "k")
	s := &connSession{siteID: "site_1", out: make(chan Message, sessionQueueLen), done: make(chan struct{})}
	h.sessions["site_1"] = s
	data := msg(MsgData, DataPayload{StreamID: "st"})
	for i := 0; i < dataHighWater; i++ {
		if err := h.SendData("site_1", data, nil, time.Second); err != nil {
			t.Fatalf("send %d: %v", i, err)
		}
	}
	if err := h.SendData("site_1", data, nil, 50*time.Millisecond); !errors.Is(err, errStreamBackpressure) {
		t.Fatalf("above high water: %v, want backpressure", err)
	}
	select {
	case <-s.done:
		t.Fatal("a busy stream disconnected the site")
	default:
	}
	if err := h.Send("site_1", msg(MsgKill, KillData{GrantID: "*"})); err != nil {
		t.Fatalf("control message after data backpressure: %v", err)
	}
	<-s.out // the connector drains one message → room again
	for len(s.out) >= dataHighWater {
		<-s.out
	}
	if err := h.SendData("site_1", data, nil, time.Second); err != nil {
		t.Fatalf("after drain: %v", err)
	}
}
