package tunnel

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/tunr-dev/tunr/internal/proxy"
)

// A read-only stream must drop the visitor's data frames but keep the
// connection usable, so server→client updates still flow.
func TestWSStreamHub_ReadOnlyDropsVisitorMessages(t *testing.T) {
	got := make(chan string, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		up := websocket.Upgrader{}
		c, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		for {
			_, msg, err := c.ReadMessage()
			if err != nil {
				return
			}
			got <- string(msg)
		}
	}))
	defer srv.Close()

	dial := func() *websocket.Conn {
		c, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http"), nil)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}

	hub := newWSStreamHub()
	hub.set("ro", dial(), true)
	hub.set("rw", dial(), false)
	defer hub.closeAll()

	if err := hub.writeFrame("ro", websocket.TextMessage, []byte("delete everything")); err != nil {
		t.Fatalf("dropped frame should not be an error: %v", err)
	}
	if err := hub.writeFrame("ro", websocket.PingMessage, nil); err != nil {
		t.Fatalf("control frames must still pass: %v", err)
	}
	if err := hub.writeFrame("rw", websocket.TextMessage, []byte("hello")); err != nil {
		t.Fatal(err)
	}

	select {
	case m := <-got:
		if m != "hello" {
			t.Fatalf("app received %q from a read-only stream", m)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("writable stream message never arrived")
	}
	select {
	case m := <-got:
		t.Fatalf("unexpected message reached the app: %q", m)
	case <-time.After(200 * time.Millisecond):
	}

	hub.shutdownStream("ro", websocket.CloseNormalClosure, "")
	if hub.readOnly["ro"] {
		t.Fatal("readOnly flag leaked after shutdown")
	}
}

// "demo": true on a tcp tunnel in .tunr.json used to be silently ignored.
func TestStart_DemoRejectedOnRawTunnels(t *testing.T) {
	m := NewManager("ws://127.0.0.1:1")
	for _, p := range []TunnelProtocol{ProtocolTCP, ProtocolUDP, ProtocolTLS} {
		_, err := m.Start(context.Background(), 5432, StartOptions{Protocol: p, DemoMode: true})
		if err == nil || !strings.Contains(err.Error(), "demo mode only works on HTTP") {
			t.Fatalf("%s: want demo rejection, got %v", p, err)
		}
	}
	if n := len(m.tunnels); n != 0 {
		t.Fatalf("rejected tunnels left in the manager: %d", n)
	}
}

// ws_open must be checked against the tunnel's password/token/IP rules the
// same way an HTTP request is.
func TestHandshakeRequest_FeedsAccessChecks(t *testing.T) {
	lp := &proxy.LocalProxy{Password: "s3cret", BearerToken: "tok"}

	withCreds := &wsOpenPayload{
		StreamID:  "s1",
		Path:      "/live?token=tok",
		HeadersV2: map[string][]string{"Authorization": {"Basic YWRtaW46czNjcmV0"}}, // admin:s3cret
	}
	// Basic auth and bearer both read Authorization; the token travels in the query.
	if !lp.AuthorizeWS(handshakeRequest(withCreds, withCreds.Path)) {
		t.Fatal("valid credentials rejected")
	}

	legacy := &wsOpenPayload{StreamID: "s2", Path: "/live?token=tok", Headers: map[string]string{"Authorization": "Basic YWRtaW46czNjcmV0"}}
	if !lp.AuthorizeWS(handshakeRequest(legacy, legacy.Path)) {
		t.Fatal("v1 Headers map not honoured")
	}

	noCreds := &wsOpenPayload{StreamID: "s3", Path: "/live"}
	if lp.AuthorizeWS(handshakeRequest(noCreds, noCreds.Path)) {
		t.Fatal("anonymous WebSocket accepted on a protected tunnel")
	}
}
