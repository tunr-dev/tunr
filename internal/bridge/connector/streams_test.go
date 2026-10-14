package connector

import (
	"context"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tunr-dev/tunr/internal/bridge"
)

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// signGrant builds a compact JWS the way the relay does.
func signGrant(priv ed25519.PrivateKey, claims map[string]any) string {
	h, _ := json.Marshal(map[string]string{"alg": "EdDSA", "typ": "JWT"})
	p, _ := json.Marshal(claims)
	in := b64(h) + "." + b64(p)
	return in + "." + b64(ed25519.Sign(priv, []byte(in)))
}

func baseClaims(stream, res, mode string) map[string]any {
	now := time.Now()
	return map[string]any{
		"iss": "relay.test", "sub": "agt_1", "aud": []string{"site_1"}, "res": res, "gid": "grt_1",
		"mode": mode, "jti": stream, "iat": now.Unix(), "nbf": now.Unix(), "exp": now.Add(15 * time.Minute).Unix(),
	}
}

func TestVerifyGrant(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	_, other, _ := ed25519.GenerateKey(nil)
	now := time.Now()
	ok := signGrant(priv, baseClaims("acs_1", "res_1", "ro"))
	if c, err := VerifyGrant(pub, ok, "site_1", now); err != nil || c.GrantID != "grt_1" || c.ID != "acs_1" {
		t.Fatalf("valid grant: %+v %v", c, err)
	}
	// aud as a plain string is accepted too.
	cl := baseClaims("acs_1", "res_1", "ro")
	cl["aud"] = "site_1"
	if _, err := VerifyGrant(pub, signGrant(priv, cl), "site_1", now); err != nil {
		t.Errorf("string aud: %v", err)
	}

	mut := func(f func(map[string]any)) string {
		c := baseClaims("acs_1", "res_1", "ro")
		f(c)
		return signGrant(priv, c)
	}
	parts := strings.Split(ok, ".")
	payload, _ := base64.RawURLEncoding.DecodeString(parts[1])
	escalated := parts[0] + "." + b64([]byte(strings.Replace(string(payload), `"ro"`, `"rw"`, 1))) + "." + parts[2]
	hsHeader := b64([]byte(`{"alg":"HS256","typ":"JWT"}`))
	mac := hmac.New(sha256.New, pub)
	mac.Write([]byte(hsHeader + "." + parts[1]))
	hs := hsHeader + "." + parts[1] + "." + b64(mac.Sum(nil))
	none := b64([]byte(`{"alg":"none"}`)) + "." + parts[1] + "."

	bad := map[string]string{
		"other key":       signGrant(other, baseClaims("acs_1", "res_1", "ro")),
		"tampered ro→rw":  escalated,
		"HS256 confusion": hs,
		"alg none":        none,
		"crit header":     b64([]byte(`{"alg":"EdDSA","crit":["x"]}`)) + "." + parts[1] + "." + parts[2],
		"other site":      mut(func(c map[string]any) { c["aud"] = []string{"site_2"} }),
		"two audiences":   mut(func(c map[string]any) { c["aud"] = []string{"site_1", "site_2"} }),
		"expired": mut(func(c map[string]any) {
			c["iat"] = now.Add(-20 * time.Minute).Unix()
			c["exp"] = now.Add(-5 * time.Minute).Unix()
		}),
		"too long":   mut(func(c map[string]any) { c["exp"] = now.Add(2 * time.Hour).Unix() }),
		"future":     mut(func(c map[string]any) { c["iat"] = now.Add(5 * time.Minute).Unix() }),
		"no gid":     mut(func(c map[string]any) { delete(c, "gid") }),
		"admin mode": mut(func(c map[string]any) { c["mode"] = "admin" }),
		"garbage":    "not.a.jwt",
	}
	for name, tok := range bad {
		if _, err := VerifyGrant(pub, tok, "site_1", now); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// echoServer is a TCP server that echoes uppercased.
func echoServer(t *testing.T) string {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				buf := make([]byte, 1024)
				for {
					n, err := c.Read(buf)
					if err != nil {
						return
					}
					_, _ = c.Write([]byte(strings.ToUpper(string(buf[:n]))))
				}
			}()
		}
	}()
	return l.Addr().String()
}

type sink struct {
	mu   sync.Mutex
	msgs []bridge.Message
	ch   chan bridge.Message
}

func newSink() *sink { return &sink{ch: make(chan bridge.Message, 64)} }
func (s *sink) send(m bridge.Message) {
	s.mu.Lock()
	s.msgs = append(s.msgs, m)
	s.mu.Unlock()
	s.ch <- m
}
func (s *sink) next(t *testing.T) bridge.Message {
	t.Helper()
	select {
	case m := <-s.ch:
		return m
	case <-time.After(3 * time.Second):
		t.Fatal("no message from connector")
	}
	return bridge.Message{}
}

func newTestManager(t *testing.T, target string) (*Manager, ed25519.PrivateKey, *[]string) {
	t.Helper()
	pub, priv, _ := ed25519.GenerateKey(nil)
	cfg := &Config{Relay: "https://bridge.tunr.sh", Site: "hq-site", RelayKey: "ed25519:" + b64(pub),
		Resources: []Resource{
			{Name: "echo", Kind: "tcp", Target: target},
			{Name: "erp-db", Kind: "postgres", Target: target, Database: "logo"},
		}}
	var dialed []string
	m, err := NewManager(cfg, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	real := m.Dial
	m.Dial = func(ctx context.Context, n, addr string) (net.Conn, error) {
		mu.Lock()
		dialed = append(dialed, addr)
		mu.Unlock()
		return real(ctx, n, addr)
	}
	m.Ready(bridge.WelcomeData{SiteID: "site_1", Resources: []bridge.WelcomeResource{
		{Name: "echo", ID: "res_echo"}, {Name: "erp-db", ID: "res_pg"},
	}})
	return m, priv, &dialed
}

func reasonOf(m bridge.Message) string {
	var c bridge.CloseData
	_ = json.Unmarshal(m.Data, &c)
	return c.Reason
}

func TestStreamRoundTripAndKill(t *testing.T) {
	addr := echoServer(t)
	m, priv, dialed := newTestManager(t, addr)
	s := newSink()

	m.Open(context.Background(), bridge.OpenData{StreamID: "acs_1", ResourceID: "res_echo",
		GrantJWT: signGrant(priv, baseClaims("acs_1", "res_echo", "rw"))}, s.send)
	// Data may arrive before the dial completes; it must be buffered, not lost.
	m.Data(bridge.DataPayload{StreamID: "acs_1", PayloadB64: base64.StdEncoding.EncodeToString([]byte("hello"))})
	got := s.next(t)
	var d bridge.DataPayload
	_ = json.Unmarshal(got.Data, &d)
	if b, _ := base64.StdEncoding.DecodeString(d.PayloadB64); got.Type != bridge.MsgData || string(b) != "HELLO" {
		t.Fatalf("got %s %q", got.Type, b)
	}
	if len(*dialed) != 1 || (*dialed)[0] != addr {
		t.Errorf("dialed %v, want only the configured target", *dialed)
	}

	m.Kill(bridge.KillData{GrantID: "grt_1"})
	if c := s.next(t); c.Type != bridge.MsgClose || reasonOf(c) != "killed" {
		t.Fatalf("kill: %s %s", c.Type, c.Data)
	}
	if m.Count() != 0 {
		t.Error("stream survived kill")
	}
}

func TestStreamRefusals(t *testing.T) {
	addr := echoServer(t)
	m, priv, dialed := newTestManager(t, addr)
	s := newSink()
	open := func(stream, res string, claims map[string]any) string {
		m.Open(context.Background(), bridge.OpenData{StreamID: stream, ResourceID: res,
			GrantJWT: signGrant(priv, claims)}, s.send)
		return reasonOf(s.next(t))
	}

	cases := []struct {
		name, stream, res, wantReason string
		claims                        map[string]any
	}{
		{"tcp with ro grant", "acs_a", "res_echo", "mode_not_enforceable", baseClaims("acs_a", "res_echo", "ro")},
		{"postgres rw", "acs_b", "res_pg", "mode_not_allowed", baseClaims("acs_b", "res_pg", "rw")},
		{"postgres without credential", "acs_b2", "res_pg", "no_credential", baseClaims("acs_b2", "res_pg", "ro")},
		{"jwt for another stream", "acs_c", "res_echo", "invalid_grant", baseClaims("acs_other", "res_echo", "rw")},
		{"jwt for another resource", "acs_d", "res_echo", "invalid_grant", baseClaims("acs_d", "res_pg", "rw")},
		{"resource not served", "acs_e", "res_unknown", "unknown_resource", baseClaims("acs_e", "res_unknown", "rw")},
	}
	for _, c := range cases {
		if got := open(c.stream, c.res, c.claims); got != c.wantReason {
			t.Errorf("%s: reason %q, want %q", c.name, got, c.wantReason)
		}
	}
	if len(*dialed) != 0 {
		t.Fatalf("refused opens dialled %v", *dialed)
	}

	// Replay: the same JWT (same jti) can't open twice, even after the first closed.
	claims := baseClaims("acs_r", "res_echo", "rw")
	tok := signGrant(priv, claims)
	m.Open(context.Background(), bridge.OpenData{StreamID: "acs_r", ResourceID: "res_echo", GrantJWT: tok}, s.send)
	time.Sleep(50 * time.Millisecond)
	m.Close(bridge.CloseData{StreamID: "acs_r"})
	m.Open(context.Background(), bridge.OpenData{StreamID: "acs_r", ResourceID: "res_echo", GrantJWT: tok}, s.send)
	if r := reasonOf(s.next(t)); r != "invalid_grant" {
		t.Errorf("replay: reason %q", r)
	}
}

func TestStreamDialFailureHidesTarget(t *testing.T) {
	m, priv, _ := newTestManager(t, "127.0.0.1:1") // nothing listens on port 1
	s := newSink()
	m.Open(context.Background(), bridge.OpenData{StreamID: "acs_1", ResourceID: "res_echo",
		GrantJWT: signGrant(priv, baseClaims("acs_1", "res_echo", "rw"))}, s.send)
	c := s.next(t)
	if reasonOf(c) != "dial_failed" || strings.Contains(string(c.Data), "127.0.0.1") {
		t.Fatalf("close = %s", c.Data)
	}
}
