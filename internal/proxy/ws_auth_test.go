package proxy

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func wsHandshake(path string, hdr map[string]string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, path, nil)
	r.Header.Set("Upgrade", "websocket")
	r.Header.Set("Connection", "Upgrade")
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	return r
}

func TestAuthorizeWS(t *testing.T) {
	wl := NewIPWhitelist([]string{"203.0.113.0/24"})

	cases := []struct {
		name string
		lp   *LocalProxy
		req  *http.Request
		want bool
	}{
		{"no checks configured", &LocalProxy{}, wsHandshake("/ws", nil), true},

		{"password missing", &LocalProxy{Password: "s3cret"}, wsHandshake("/ws", nil), false},
		{"password wrong", &LocalProxy{Password: "s3cret"},
			wsHandshake("/ws", map[string]string{"Authorization": "Basic YWRtaW46bm9wZQ=="}), false}, // admin:nope
		{"password right", &LocalProxy{Password: "s3cret"},
			wsHandshake("/ws", map[string]string{"Authorization": "Basic YWRtaW46czNjcmV0"}), true}, // admin:s3cret

		{"token missing", &LocalProxy{BearerToken: "tok"}, wsHandshake("/ws", nil), false},
		{"token in header", &LocalProxy{BearerToken: "tok"},
			wsHandshake("/ws", map[string]string{"Authorization": "Bearer tok"}), true},
		// Browsers can't set headers on a WebSocket, so ?token= is the usual way.
		{"token in query", &LocalProxy{BearerToken: "tok"}, wsHandshake("/ws?token=tok", nil), true},

		{"ip outside allowlist", &LocalProxy{IPWhitelist: &wl},
			wsHandshake("/ws", map[string]string{"X-Forwarded-For": "198.51.100.1"}), false},
		{"ip inside allowlist", &LocalProxy{IPWhitelist: &wl},
			wsHandshake("/ws", map[string]string{"X-Forwarded-For": "203.0.113.9"}), true},

		{"all checks, one fails", &LocalProxy{IPWhitelist: &wl, BearerToken: "tok"},
			wsHandshake("/ws?token=tok", map[string]string{"X-Forwarded-For": "198.51.100.1"}), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.lp.AuthorizeWS(tc.req); got != tc.want {
				t.Fatalf("AuthorizeWS = %v, want %v", got, tc.want)
			}
		})
	}
}

// The direct WebSocket path in LocalProxy.ServeHTTP must answer 401 before it
// ever dials the dev server.
func TestServeHTTP_WebSocketRequiresPassword(t *testing.T) {
	dialed := false
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		dialed = true
	}))
	defer upstream.Close()

	p, err := NewLocalProxy(mustPortFromURL(t, upstream.URL), nil)
	if err != nil {
		t.Fatal(err)
	}
	p.Password = "s3cret"
	p.BuildMiddlewareChain()

	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, wsHandshake("/ws", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", rec.Code)
	}
	if dialed {
		t.Fatal("dev server was reached without credentials")
	}
}
