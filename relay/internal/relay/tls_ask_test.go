package relay

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestTLSAsk(t *testing.T) {
	h := TLSAskHandler("tunnel.example.com")
	cases := map[string]int{
		"tunnel.example.com":          http.StatusOK,
		"TUNNEL.example.com.":         http.StatusOK,
		"myapp.tunnel.example.com":    http.StatusOK,
		"my-app-2.tunnel.example.com": http.StatusOK,
		"a.b.tunnel.example.com":      http.StatusNotFound,
		"evil.com":                    http.StatusNotFound,
		"tunnel.example.com.evil.com": http.StatusNotFound,
		"xtunnel.example.com":         http.StatusNotFound,
		".tunnel.example.com":         http.StatusNotFound,
		"my_app.tunnel.example.com":   http.StatusNotFound,
		"":                            http.StatusNotFound,
	}
	for host, want := range cases {
		rec := httptest.NewRecorder()
		h(rec, httptest.NewRequest(http.MethodGet, "/api/v1/tls/ask?domain="+host, nil))
		if rec.Code != want {
			t.Errorf("%q: got %d, want %d", host, rec.Code, want)
		}
	}
}
