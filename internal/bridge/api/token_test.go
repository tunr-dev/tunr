package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestTokenSource(t *testing.T) {
	var calls atomic.Int32
	ttl := atomic.Int64{}
	ttl.Store(int64(time.Hour))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in struct{ Bootstrap string }
		_ = json.NewDecoder(r.Body).Decode(&in)
		if r.URL.Path != "/api/v1/bridge/agent/token" || in.Bootstrap != "tunr_boot_ok" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		n := calls.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{"token": fmt.Sprintf("tok-%d", n),
			"expires_at": time.Now().Add(time.Duration(ttl.Load()))})
	}))
	defer srv.Close()
	ctx := context.Background()

	ts := &TokenSource{Relay: srv.URL, Bootstrap: "tunr_boot_ok"}
	a, err := ts.Token(ctx)
	b, _ := ts.Token(ctx)
	if err != nil || a != "tok-1" || b != "tok-1" || calls.Load() != 1 {
		t.Fatalf("cached token: %q %q %v calls=%d", a, b, err, calls.Load())
	}
	// A token close to expiry is renewed.
	ts.exp = time.Now().Add(2 * time.Minute)
	if c, _ := ts.Token(ctx); c != "tok-2" {
		t.Errorf("near-expiry token not renewed: %q", c)
	}

	if s, _ := (&TokenSource{Static: "fixed"}).Token(ctx); s != "fixed" {
		t.Errorf("static: %q", s)
	}
	_, err = (&TokenSource{Relay: srv.URL, Bootstrap: "tunr_boot_rotated"}).Token(ctx)
	if err == nil || !strings.Contains(err.Error(), "redeployed or disabled") {
		t.Errorf("refused exchange: %v", err)
	}
	if _, err := (&TokenSource{}).Token(ctx); err == nil {
		t.Error("empty source returned a token")
	}

	// The API client uses the source.
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"auth": r.Header.Get("Authorization")})
	}))
	defer api.Close()
	var out map[string]string
	if err := (&Client{Relay: api.URL, Tokens: ts}).Do(ctx, "GET", "/agent/me", nil, &out); err != nil || out["auth"] != "Bearer tok-2" {
		t.Errorf("client with token source: %v %v", out, err)
	}
}
