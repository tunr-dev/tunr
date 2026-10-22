package bridge

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tunr-dev/tunr/relay/internal/auth"
)

func TestConfigFromEnv(t *testing.T) {
	cases := []struct {
		v    string
		want bool
	}{
		{"", false},
		{"0", false},
		{"false", false},
		{"yes", false}, // only 1/true enable; anything else stays off
		{"1", true},
		{"true", true},
		{"TRUE", true},
		{" 1 ", true},
	}
	for _, c := range cases {
		v, want := c.v, c.want
		got := ConfigFromEnv(func(k string) string {
			if k == EnvEnabled {
				return v
			}
			return ""
		}).Enabled
		if got != want {
			t.Errorf("%s=%q: Enabled=%v, want %v", EnvEnabled, v, got, want)
		}
	}
}

// With the flag off the bridge must mount nothing: requests to its paths reach
// whatever handled them before (here a sentinel standing in for the tunnel
// proxy's "/" catch-all).
func TestDisabledMountsNothing(t *testing.T) {
	mux := http.NewServeMux()
	s, err := New(Config{Enabled: false}, Deps{})
	if err != nil {
		t.Fatalf("disabled New must not fail: %v", err)
	}
	s.RegisterRoutes(mux)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})

	for _, path := range []string{"/api/v1/bridge/health", "/bridge/connect", "/bridge/access"} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusTeapot {
			t.Errorf("%s: got %d, want fall-through to catch-all", path, rec.Code)
		}
	}
}

func TestEnabledHealth(t *testing.T) {
	mux := http.NewServeMux()
	signer, _, _ := auth.GenerateGrantSigner("test")
	(&Server{cfg: Config{Enabled: true}, signer: signer}).RegisterRoutes(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/bridge/health", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("health: got %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q", ct)
	}
	if !strings.Contains(rec.Body.String(), signer.KeyString()) {
		t.Errorf("health does not expose relay key: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"hosted_agents":false`) {
		t.Errorf("health does not report hosted agents off: %s", rec.Body.String())
	}

	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/bridge/health", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST health: got %d, want 405", rec.Code)
	}
}

// Misconfiguration with the flag on must fail loudly at boot, never degrade
// into an unsafe mode.
func TestEnabledRequiresDBAndKey(t *testing.T) {
	j, _ := auth.NewJWTAuth("supersecret-32-characters-minimum!!", 0)
	if _, err := New(Config{Enabled: true, SigningKey: "x"}, Deps{JWT: j}); err == nil {
		t.Error("enabled without DB: want error")
	}
	cfg := ConfigFromEnv(func(k string) string {
		return map[string]string{EnvEnabled: "1", "TUNR_DOMAIN": "example.test"}[k]
	})
	if cfg.PublicURL != "https://relay.example.test" || cfg.Issuer != "example.test" || cfg.SigningKey != "" {
		t.Errorf("cfg = %+v", cfg)
	}
}
