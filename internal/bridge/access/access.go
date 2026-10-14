// Package access is `tunr access` (agent side, P3-T3): a local TCP listener
// on 127.0.0.1 whose every connection becomes one authorised bridge stream to
// a customer resource. The agent talks to localhost; it never learns where
// the resource lives.
package access

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// Error is a relay refusal (HTTP status + machine code).
type Error struct {
	Status  int
	Code    string
	Message string
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

// Advice turns a refusal code into the next step for the operator.
func (e *Error) Advice(resource string) string {
	switch e.Code {
	case "no_grant":
		return "ask the customer to approve access to " + resource + " (tunr access request comes with grants, P4)"
	case "site_offline":
		return "the customer's connector is offline; they need to start `tunr connect`"
	case "unknown_resource":
		return "check the address with the catalog; the customer may not have linked your org"
	case "invalid_token", "auth_required":
		return "set a valid agent token (TUNR_AGENT_TOKEN or --token-stdin)"
	}
	return ""
}

// Check is the relay's pre-flight answer.
type Check struct {
	Resource       string    `json:"resource"`
	Kind           string    `json:"kind"`
	GrantID        string    `json:"grant_id"`
	Mode           string    `json:"mode"`
	GrantExpiresAt time.Time `json:"grant_expires_at"`
}

// Client opens streams for one resource.
type Client struct {
	Relay    string // https://… (ws scheme derived)
	Token    string // agent token; never logged
	Resource string // "<customer>/<name>"
	GrantID  string // optional
	Logf     func(string, ...any)
	HTTP     *http.Client
}

func (c *Client) logf(f string, a ...any) {
	if c.Logf != nil {
		c.Logf(f, a...)
	}
}

func (c *Client) endpoint(path string, ws bool) string {
	base := strings.TrimRight(c.Relay, "/")
	if ws {
		base = strings.Replace(strings.Replace(base, "https://", "wss://", 1), "http://", "ws://", 1)
	}
	q := url.Values{"resource": {c.Resource}}
	if c.GrantID != "" {
		q.Set("grant", c.GrantID)
	}
	return base + path + "?" + q.Encode()
}

func decodeRefusal(resp *http.Response) error {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	var body struct{ Error, Message string }
	if json.Unmarshal(b, &body) != nil || body.Error == "" {
		return &Error{Status: resp.StatusCode, Code: "http_" + fmt.Sprint(resp.StatusCode), Message: strings.TrimSpace(string(b))}
	}
	return &Error{Status: resp.StatusCode, Code: body.Error, Message: body.Message}
}

// Check asks the relay whether a stream would be allowed, without opening one.
func (c *Client) Check(ctx context.Context) (*Check, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.endpoint("/bridge/access/check", false), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 15 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, decodeRefusal(resp)
	}
	var out Check
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Serve accepts on l until ctx ends; each connection is bridged separately.
func (c *Client) Serve(ctx context.Context, l net.Listener) error {
	go func() {
		<-ctx.Done()
		_ = l.Close()
	}()
	var wg sync.WaitGroup
	defer wg.Wait()
	for {
		local, err := l.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := c.Bridge(ctx, local); err != nil {
				var ae *Error
				if errors.As(err, &ae) {
					c.logf("connection refused by relay: %s — %s", ae.Code, ae.Advice(c.Resource))
				} else {
					c.logf("connection ended: %v", err)
				}
			}
		}()
	}
}

// Bridge carries one local connection over one relay stream.
func (c *Client) Bridge(ctx context.Context, local net.Conn) error {
	defer local.Close()
	hdr := http.Header{"Authorization": {"Bearer " + c.Token}}
	d := websocket.Dialer{HandshakeTimeout: 15 * time.Second, Proxy: http.ProxyFromEnvironment}
	ws, resp, err := d.DialContext(ctx, c.endpoint("/bridge/access", true), hdr)
	if resp != nil {
		if err != nil && resp.StatusCode != http.StatusSwitchingProtocols {
			rerr := decodeRefusal(resp)
			_ = resp.Body.Close()
			return rerr
		}
		_ = resp.Body.Close()
	}
	if err != nil {
		return fmt.Errorf("dial relay: %w", err)
	}
	defer ws.Close()
	ws.SetReadLimit(1 << 20)

	// The relay pings every 30s; anything silent for 90s is dead.
	_ = ws.SetReadDeadline(time.Now().Add(90 * time.Second))
	ws.SetPingHandler(func(data string) error {
		_ = ws.SetReadDeadline(time.Now().Add(90 * time.Second))
		return ws.WriteControl(websocket.PongMessage, []byte(data), time.Now().Add(5*time.Second))
	})

	done := make(chan error, 2)
	var streamID string

	// relay → local
	go func() {
		for {
			typ, b, err := ws.ReadMessage()
			if err != nil {
				done <- nil
				return
			}
			_ = ws.SetReadDeadline(time.Now().Add(90 * time.Second))
			switch typ {
			case websocket.BinaryMessage:
				if _, err := local.Write(b); err != nil {
					done <- nil
					return
				}
			case websocket.TextMessage:
				var m struct {
					Type string `json:"type"`
					Data struct {
						StreamID string `json:"stream_id"`
						GrantID  string `json:"grant_id"`
						Reason   string `json:"reason"`
					} `json:"data"`
				}
				if json.Unmarshal(b, &m) != nil {
					continue
				}
				switch m.Type {
				case "access_ready":
					streamID = m.Data.StreamID
					c.logf("stream %s open (grant %s)", m.Data.StreamID, m.Data.GrantID)
				case "access_close":
					c.logf("stream %s closed: %s", streamID, m.Data.Reason)
					done <- nil
					return
				}
			}
		}
	}()

	// local → relay (single writer of data frames)
	go func() {
		buf := make([]byte, 32<<10)
		for {
			n, err := local.Read(buf)
			if n > 0 {
				_ = ws.SetWriteDeadline(time.Now().Add(30 * time.Second))
				if werr := ws.WriteMessage(websocket.BinaryMessage, buf[:n]); werr != nil {
					done <- nil
					return
				}
			}
			if err != nil {
				_ = ws.SetWriteDeadline(time.Now().Add(5 * time.Second))
				_ = ws.WriteMessage(websocket.TextMessage, []byte(`{"type":"access_close","data":{"reason":"client"}}`))
				done <- nil
				return
			}
		}
	}()

	select {
	case <-ctx.Done():
	case <-done:
	}
	_ = ws.WriteControl(websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(time.Second))
	return nil
}
