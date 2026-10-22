package relay

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gorilla/websocket"
)

// Refusals must be plain HTTP responses, before any WebSocket upgrade.
func TestServeBrowserTCPRefusesBeforeUpgrade(t *testing.T) {
	reg := NewRegistry()
	h := NewHandler(reg, nil, nil, "tunr.test")
	srv := httptest.NewServer(http.HandlerFunc(h.ServeBrowserTCP))
	defer srv.Close()
	if _, err := reg.RegisterWithProtocol("u1", "webapp", "http", ""); err != nil {
		t.Fatal(err)
	}
	base := "ws" + strings.TrimPrefix(srv.URL, "http") + "/tunnel/tcp"
	for _, c := range []struct {
		query string
		want  int
	}{
		{"", http.StatusBadRequest},                  // no subdomain
		{"?subdomain=nope", http.StatusNotFound},     // unknown tunnel
		{"?subdomain=webapp", http.StatusBadRequest}, // not a TCP tunnel
	} {
		conn, resp, err := websocket.DefaultDialer.Dial(base+c.query, nil)
		if err == nil {
			conn.Close()
			t.Errorf("%q: upgraded, want HTTP %d", c.query, c.want)
			continue
		}
		if resp == nil || resp.StatusCode != c.want {
			t.Errorf("%q: resp=%v err=%v, want HTTP %d", c.query, resp, err, c.want)
		}
	}
}
