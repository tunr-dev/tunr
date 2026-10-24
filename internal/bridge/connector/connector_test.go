package connector

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/tunr-dev/tunr/internal/bridge"
)

func keyString(pub ed25519.PublicKey) string {
	return "ed25519:" + base64.RawURLEncoding.EncodeToString(pub)
}

// fakeRelay is a minimal /bridge/connect peer that records everything the
// connector sends.
type fakeRelay struct {
	t        *testing.T
	srv      *httptest.Server
	key      string // relay_key sent in welcome
	token    string // accepted site token
	reject   *bridge.ErrorData
	onSess   func(c *websocket.Conn) // runs after welcome; returning closes the conn
	mu       sync.Mutex
	raw      []string // every frame received, verbatim
	sessions atomic.Int32
}

func newFakeRelay(t *testing.T, key string) *fakeRelay {
	f := &fakeRelay{t: t, key: key, token: "tunr_site_good"}
	up := websocket.Upgrader{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/bridge/connect" || r.Header.Get("Authorization") != "Bearer "+f.token {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		c, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		f.sessions.Add(1)
		_, hello, err := c.ReadMessage()
		if err != nil {
			return
		}
		f.record(string(hello))
		if f.reject != nil {
			_ = c.WriteJSON(bridge.NewMessage(bridge.MsgError, f.reject))
			return
		}
		_ = c.WriteJSON(bridge.NewMessage(bridge.MsgWelcome, bridge.WelcomeData{
			SiteID: "site_1", SiteName: "istanbul-ofis", RelayKey: f.key,
		}))
		if f.onSess != nil {
			f.onSess(c)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeRelay) record(s string) {
	f.mu.Lock()
	f.raw = append(f.raw, s)
	f.mu.Unlock()
}

func (f *fakeRelay) all() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return strings.Join(f.raw, "\n")
}

func testConfig(t *testing.T, relay, key string) *Config {
	t.Setenv(EnvInsecureRelay, "1")
	c := &Config{
		Relay: relay, Site: "istanbul-ofis", RelayKey: key,
		Resources: []Resource{
			{Name: "erp-db", Kind: "postgres", Target: "10.77.0.5:6543", Database: "logo_secret_db", Credential: "cred-erp"},
			{Name: "nas", Kind: "tcp", Target: "files.internal.acme:4455"},
		},
	}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	return c
}

func newClient(cfg *Config, token string) *Client {
	return &Client{Cfg: cfg, Token: token, Version: "test", HelloTimeout: 2 * time.Second,
		ReadTimeout: 2 * time.Second, KillPoll: 20 * time.Millisecond, MaxBackoff: 50 * time.Millisecond}
}

// The central P2 guarantee: nothing about where a resource lives ever reaches
// the relay — only names and kinds.
func TestHelloNeverLeaksTargets(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(nil)
	f := newFakeRelay(t, keyString(pub))
	ready := make(chan bridge.WelcomeData, 1)
	f.onSess = func(c *websocket.Conn) { time.Sleep(100 * time.Millisecond) }

	cl := newClient(testConfig(t, f.srv.URL, keyString(pub)), f.token)
	cl.OnReady = func(w bridge.WelcomeData) { ready <- w }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = cl.Run(ctx) }()

	select {
	case w := <-ready:
		if w.SiteID != "site_1" {
			t.Fatalf("welcome %+v", w)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("connector never became ready")
	}
	sent := f.all()
	for _, secret := range []string{"10.77.0.5", "6543", "files.internal.acme", "4455", "logo_secret_db", "cred-erp", "target", "database"} {
		if strings.Contains(sent, secret) {
			t.Errorf("relay received %q: %s", secret, sent)
		}
	}
	var m bridge.Message
	_ = json.Unmarshal([]byte(strings.Split(sent, "\n")[0]), &m)
	var h bridge.HelloData
	_ = json.Unmarshal(m.Data, &h)
	if m.Type != bridge.MsgHello || len(h.Resources) != 2 || h.Resources[0].Name != "erp-db" || h.Resources[0].Kind != "postgres" || h.Resources[0].Profile != "" {
		t.Errorf("hello = %s", sent)
	}
}

func TestKeyMismatchStops(t *testing.T) {
	pinned, _, _ := ed25519.GenerateKey(nil)
	other, _, _ := ed25519.GenerateKey(nil)
	f := newFakeRelay(t, keyString(other))
	err := newClient(testConfig(t, f.srv.URL, keyString(pinned)), f.token).Run(context.Background())
	if !errors.Is(err, ErrKeyMismatch) {
		t.Fatalf("Run = %v, want ErrKeyMismatch", err)
	}
	if n := f.sessions.Load(); n != 1 {
		t.Errorf("retried after key mismatch: %d sessions", n)
	}
}

func TestUnauthorizedAndRejectedStop(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(nil)
	f := newFakeRelay(t, keyString(pub))
	if err := newClient(testConfig(t, f.srv.URL, keyString(pub)), "tunr_site_bad").Run(context.Background()); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("bad token: %v", err)
	}
	f.reject = &bridge.ErrorData{Code: "resource_conflict", Message: "erp-db is served by another site"}
	err := newClient(testConfig(t, f.srv.URL, keyString(pub)), f.token).Run(context.Background())
	var rej *RejectedError
	if !errors.As(err, &rej) || rej.Code != "resource_conflict" {
		t.Fatalf("rejected hello: %v", err)
	}
}

func TestPingAndRefusedOpen(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(nil)
	f := newFakeRelay(t, keyString(pub))
	got := make(chan []bridge.Message, 1)
	f.onSess = func(c *websocket.Conn) {
		_ = c.WriteJSON(bridge.Message{Type: bridge.MsgPing})
		_ = c.WriteJSON(bridge.Message{Type: "future_type_from_newer_relay"})
		_ = c.WriteJSON(bridge.NewMessage(bridge.MsgOpen, bridge.OpenData{StreamID: "s1", ResourceID: "res_1", GrantJWT: "x"}))
		var msgs []bridge.Message
		for len(msgs) < 2 {
			var m bridge.Message
			_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
			if c.ReadJSON(&m) != nil {
				break
			}
			msgs = append(msgs, m)
		}
		got <- msgs
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = newClient(testConfig(t, f.srv.URL, keyString(pub)), f.token).Run(ctx) }()
	msgs := <-got
	if len(msgs) != 2 || msgs[0].Type != bridge.MsgPong || msgs[1].Type != bridge.MsgClose ||
		!strings.Contains(string(msgs[1].Data), "unsupported") {
		t.Fatalf("got %+v", msgs)
	}
}

func TestReconnectAndKillSwitch(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(nil)
	f := newFakeRelay(t, keyString(pub))
	hold := make(chan struct{})
	f.onSess = func(c *websocket.Conn) {
		if f.sessions.Load() == 1 {
			return // drop the first session at once → connector must reconnect
		}
		select {
		case <-hold:
		case <-time.After(5 * time.Second):
		}
	}
	kill := filepath.Join(t.TempDir(), "connector.killed")
	cl := newClient(testConfig(t, f.srv.URL, keyString(pub)), f.token)
	cl.KillPath = kill
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- cl.Run(ctx) }()

	waitFor(t, "reconnect", func() bool { return f.sessions.Load() >= 2 })

	_ = os.WriteFile(kill, nil, 0o600)
	time.Sleep(200 * time.Millisecond)
	n := f.sessions.Load()
	time.Sleep(200 * time.Millisecond)
	if f.sessions.Load() != n {
		t.Fatal("connector kept connecting while killed")
	}
	_ = os.Remove(kill)
	waitFor(t, "resume after kill switch released", func() bool { return f.sessions.Load() > n })
	close(hold)
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Errorf("Run = %v", err)
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for i := 0; i < 150; i++ {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out: %s", what)
}

func TestConfigValidation(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(nil)
	good := func() *Config {
		return &Config{Relay: "https://bridge.tunr.sh", Site: "hq-site", RelayKey: keyString(pub),
			Resources: []Resource{{Name: "erp-db", Kind: "postgres", Target: "10.0.0.5:5432", Database: "logo"}}}
	}
	if err := good().Validate(); err != nil {
		t.Fatalf("good config: %v", err)
	}
	bad := map[string]func(c *Config){
		"http relay":    func(c *Config) { c.Relay = "http://bridge.tunr.sh" },
		"private relay": func(c *Config) { c.Relay = "https://10.0.0.1" },
		"no relay key":  func(c *Config) { c.RelayKey = "" },
		"rsa relay key": func(c *Config) { c.RelayKey = "rsa:abc" },
		"bad site":      func(c *Config) { c.Site = "Bad Site" },
		"bad target":    func(c *Config) { c.Resources[0].Target = "10.0.0.5" },
		"port 0":        func(c *Config) { c.Resources[0].Target = "10.0.0.5:0" },
		"bad kind":      func(c *Config) { c.Resources[0].Kind = "ssh" },
		"pg without db": func(c *Config) { c.Resources[0].Database = "" },
		"dup resource":  func(c *Config) { c.Resources = append(c.Resources, c.Resources[0]) },
		"bad store":     func(c *Config) { c.SecretStore = "plaintext" },
	}
	for name, mut := range bad {
		c := good()
		mut(c)
		if err := c.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}

	// Round trip through disk; unknown fields are refused on load.
	path := filepath.Join(t.TempDir(), "tunr", "connector.json")
	if err := good().Save(path); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(path); runtime.GOOS != "windows" && st.Mode().Perm() != 0o600 { // no Unix modes on Windows
		t.Errorf("config mode %o", st.Mode().Perm())
	}
	if c, err := Load(path); err != nil || c.Resources[0].Target != "10.0.0.5:5432" {
		t.Fatalf("Load = %+v, %v", c, err)
	}
	b, _ := os.ReadFile(path)
	_ = os.WriteFile(path, []byte(strings.Replace(string(b), `"site"`, `"password": "x", "site"`, 1)), 0o600)
	if _, err := Load(path); err == nil {
		t.Error("unknown field accepted")
	}
}
