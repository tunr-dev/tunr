package relay

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestTunnelLabel(t *testing.T) {
	cases := map[string]string{
		"myapp.tunr.sh":      "myapp",
		"MyApp.tunr.sh:443":  "myapp",
		"a.b.tunr.sh":        "a.b",
		"tunr.sh":            "",
		"relay.tunr.sh":      "",
		"bridge.tunr.sh":     "",
		"www.tunr.sh":        "",
		"connect.tunr.sh":    "",
		"relay:8080":         "",
		"127.0.0.1:8080":     "",
		"evil.com":           "",
		"myapp.tunr.sh.evil": "",
	}
	for host, want := range cases {
		if got := TunnelLabel(host, "tunr.sh", "connect"); got != want {
			t.Errorf("TunnelLabel(%q) = %q, want %q", host, got, want)
		}
	}
}

// A tunnel's own /api/v1/health, /portal/ and /auth/verify must reach the
// tunnel, not the relay; /tunnel/tcp is whatever the tunnel handler does.
func TestSplitByHost(t *testing.T) {
	tag := func(name string) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(name)) })
	}
	h := SplitByHost("tunr.sh", tag("relay"), tag("tunnel"))
	cases := []struct{ host, path, want string }{
		{"myapp.tunr.sh", "/api/v1/health", "tunnel"},
		{"myapp.tunr.sh", "/portal/", "tunnel"},
		{"myapp.tunr.sh", "/auth/verify", "tunnel"},
		{"relay.tunr.sh", "/tunnel/connect", "relay"},
		{"bridge.tunr.sh", "/api/v1/bridge/health", "relay"},
		{"tunr.sh", "/api/v1/health", "relay"},
		{"relay:8080", "/api/v1/bridge/me", "relay"},
	}
	for _, c := range cases {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "http://"+c.host+c.path, nil)
		h.ServeHTTP(rec, req)
		if got := rec.Body.String(); got != c.want {
			t.Errorf("%s%s → %s, want %s", c.host, c.path, got, c.want)
		}
	}
}
