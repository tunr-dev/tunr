package access

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// fakeRelay answers /bridge/access/check and /bridge/access: refuses unless
// the token is "good", otherwise echoes binary frames uppercased.
func fakeRelay(t *testing.T) *httptest.Server {
	up := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer good" {
			w.WriteHeader(http.StatusForbidden)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "no_grant", "message": "nope"})
			return
		}
		if r.URL.Query().Get("resource") != "acme/echo" {
			t.Errorf("resource = %q", r.URL.Query().Get("resource"))
		}
		if r.URL.Path == "/bridge/access/check" {
			_ = json.NewEncoder(w).Encode(Check{Kind: "tcp", Mode: "rw", GrantID: "grt_1"})
			return
		}
		c, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		_ = c.WriteJSON(map[string]any{"type": "access_ready", "data": map[string]string{"stream_id": "acs_1", "grant_id": "grt_1"}})
		for {
			typ, b, err := c.ReadMessage()
			if err != nil {
				return
			}
			if typ == websocket.TextMessage {
				return // access_close from the agent
			}
			if string(b) == "bye" {
				_ = c.WriteJSON(map[string]any{"type": "access_close", "data": map[string]string{"reason": "revoked"}})
				return
			}
			_ = c.WriteMessage(websocket.BinaryMessage, []byte(strings.ToUpper(string(b))))
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestCheckAndRefusal(t *testing.T) {
	srv := fakeRelay(t)
	ok := &Client{Relay: srv.URL, Token: "good", Resource: "acme/echo"}
	chk, err := ok.Check(context.Background())
	if err != nil || chk.Mode != "rw" || chk.GrantID != "grt_1" {
		t.Fatalf("Check = %+v, %v", chk, err)
	}
	bad := &Client{Relay: srv.URL, Token: "bad", Resource: "acme/echo"}
	_, err = bad.Check(context.Background())
	var ae *Error
	if !errors.As(err, &ae) || ae.Code != "no_grant" || ae.Status != 403 || ae.Advice("acme/echo") == "" {
		t.Fatalf("refusal = %v", err)
	}
}

func TestServeBridgesLocalConnections(t *testing.T) {
	srv := fakeRelay(t)
	cl := &Client{Relay: srv.URL, Token: "good", Resource: "acme/echo", Logf: t.Logf}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = cl.Serve(ctx, l) }()

	c, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(3 * time.Second))
	_, _ = c.Write([]byte("select 1"))
	buf := make([]byte, 64)
	n, err := c.Read(buf)
	if err != nil || string(buf[:n]) != "SELECT 1" {
		t.Fatalf("read %q, %v", buf[:n], err)
	}
	// A relay-side close (revocation) closes the local connection.
	_, _ = c.Write([]byte("bye"))
	if _, err := c.Read(buf); err == nil {
		t.Fatal("local connection still open after access_close")
	}

	// A refused stream closes the local connection without data.
	bad := &Client{Relay: srv.URL, Token: "bad", Resource: "acme/echo", Logf: t.Logf}
	l2, _ := net.Listen("tcp", "127.0.0.1:0")
	go func() { _ = bad.Serve(ctx, l2) }()
	c2, _ := net.Dial("tcp", l2.Addr().String())
	defer c2.Close()
	_ = c2.SetDeadline(time.Now().Add(3 * time.Second))
	if n, err := c2.Read(buf); err == nil {
		t.Fatalf("refused stream returned %q", buf[:n])
	}
}
