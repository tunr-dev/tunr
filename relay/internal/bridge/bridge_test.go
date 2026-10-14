package bridge

import (
	"net/http"
	"net/http/httptest"
	"testing"
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
	New(Config{Enabled: false}).RegisterRoutes(mux)
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
	New(Config{Enabled: true}).RegisterRoutes(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/bridge/health", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("health: got %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q", ct)
	}

	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/bridge/health", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST health: got %d, want 405", rec.Code)
	}
}
