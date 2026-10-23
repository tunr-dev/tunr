// Package access is `tunr access` (agent side, P3-T3): a local TCP listener
// on 127.0.0.1 whose every connection becomes one authorised bridge stream to
// a customer resource. The agent talks to localhost; it never learns where
// the resource lives.
package access

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
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
		return "ask for access first: tunr access request " + resource + ` --reason "…"`
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
	E2EAvailable   bool      `json:"e2e_available"`
	Resource       string    `json:"resource"`
	Kind           string    `json:"kind"`
	GrantID        string    `json:"grant_id"`
	Mode           string    `json:"mode"`
	GrantExpiresAt time.Time `json:"grant_expires_at"`
}

// Client opens streams for one resource.
type Client struct {
	Relay string // https://… (ws scheme derived)
	Token string // agent token; never logged
	// TokenFunc, when set, supplies the token per request (hosted agents renew
	// theirs hourly); it wins over Token.
	TokenFunc func(ctx context.Context) (string, error)
	Resource  string // "<customer>/<name>"
	GrantID   string // optional
	// ClientSecret is the password local Postgres clients must present (the
	// DSN's password). Only its SHA-256 is sent; the connector's pgproxy
	// checks it, so other local users can't use this port.
	ClientSecret string
	// SameUserOnly refuses local connections from processes of other users
	// (Linux; see PeerCheckSupported). Raw TCP has no password of its own,
	// so this is what keeps the port to the agent's user.
	SameUserOnly bool
	// RequireE2E refuses streams the connector can't end-to-end encrypt;
	// DisableE2E never asks for it (tests, debugging).
	RequireE2E bool
	DisableE2E bool
	Logf       func(string, ...any)
	HTTP       *http.Client
}

func (c *Client) logf(f string, a ...any) {
	if c.Logf != nil {
		c.Logf(f, a...)
	}
}

func (c *Client) token(ctx context.Context) (string, error) {
	if c.TokenFunc != nil {
		return c.TokenFunc(ctx)
	}
	return c.Token, nil
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
	tok, err := c.token(ctx)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
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
		if c.SameUserOnly {
			if ok, err := peerAllowed(local); !ok {
				c.logf("local connection from %s refused: not this user's process (%v)", local.RemoteAddr(), err)
				_ = local.Close()
				continue
			}
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

// Bridge carries one local connection over one relay stream. When the site
// supports it the stream is end-to-end encrypted: a TLS 1.3 session with the
// connector, pinned to the certificate fingerprint the relay reported.
func (c *Client) Bridge(ctx context.Context, local net.Conn) error {
	defer local.Close()
	tok, err := c.token(ctx)
	if err != nil {
		return err
	}
	hdr := http.Header{"Authorization": {"Bearer " + tok}}
	if c.ClientSecret != "" {
		sum := sha256.Sum256([]byte(c.ClientSecret))
		hdr.Set("X-Tunr-Client-Secret-Sha256", hex.EncodeToString(sum[:]))
	}
	if !c.DisableE2E {
		hdr.Set("X-Tunr-E2E", "1")
	}
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
	stream := newWSConn(ws)
	defer stream.Close()

	ready, err := stream.ready()
	if err != nil {
		return err
	}
	var conn net.Conn = stream
	switch {
	case ready.E2E:
		tc := tls.Client(stream, PinnedTLS(ready.SiteCertSHA256))
		_ = tc.SetDeadline(time.Now().Add(15 * time.Second))
		if err := tc.HandshakeContext(ctx); err != nil {
			if r := stream.reason(); r != "" {
				// The connector refused the stream (policy), not the TLS.
				c.logf("stream %s closed: %s", ready.StreamID, r)
				return fmt.Errorf("stream refused by the customer's connector: %s", r)
			}
			return fmt.Errorf("end-to-end TLS with the connector failed: %w", err)
		}
		_ = tc.SetDeadline(time.Time{})
		conn = tc
		c.logf("stream %s open (grant %s, end-to-end encrypted to site certificate sha256 %s)", ready.StreamID, ready.GrantID, ready.SiteCertSHA256)
	case c.RequireE2E:
		return errors.New("the customer's connector does not support end-to-end encryption (update it) and --require-e2e is set")
	default:
		c.logf("stream %s open (grant %s) — NOT end-to-end encrypted: the customer's connector is too old", ready.StreamID, ready.GrantID)
	}

	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(conn, local); _ = closeWrite(conn); done <- struct{}{} }()
	go func() { _, _ = io.Copy(local, conn); done <- struct{}{} }()
	select {
	case <-ctx.Done():
	case <-done:
	}
	if r := stream.reason(); r != "" {
		c.logf("stream %s closed: %s", ready.StreamID, r)
	}
	return nil
}

func closeWrite(c net.Conn) error {
	if tc, ok := c.(*tls.Conn); ok {
		return tc.CloseWrite()
	}
	if w, ok := c.(*wsConn); ok {
		return w.Close()
	}
	return nil
}

// PinnedTLS trusts exactly one certificate: the connector's, by SHA-256.
func PinnedTLS(fp string) *tls.Config {
	return &tls.Config{
		MinVersion:         tls.VersionTLS13,
		ServerName:         "tunr-site",
		InsecureSkipVerify: true, // #nosec G402 -- no CA: VerifyConnection pins the exact certificate
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return errors.New("connector sent no certificate")
			}
			sum := sha256.Sum256(cs.PeerCertificates[0].Raw)
			if !strings.EqualFold(hex.EncodeToString(sum[:]), fp) {
				return errors.New("connector certificate does not match the site fingerprint")
			}
			return nil
		},
	}
}

// wsConn adapts the agent leg of a bridge stream (binary = bytes, text =
// control) to a net.Conn so TLS can run over it.
type wsConn struct {
	ws      *websocket.Conn
	buf     []byte
	wmu     sync.Mutex
	once    sync.Once
	rmu     sync.Mutex
	closeRs string
}

type readyMsg struct {
	StreamID       string `json:"stream_id"`
	GrantID        string `json:"grant_id"`
	E2E            bool   `json:"e2e"`
	SiteCertSHA256 string `json:"site_cert_sha256"`
}

func newWSConn(ws *websocket.Conn) *wsConn {
	ws.SetReadLimit(1 << 20)
	_ = ws.SetReadDeadline(time.Now().Add(90 * time.Second))
	ws.SetPingHandler(func(data string) error {
		_ = ws.SetReadDeadline(time.Now().Add(90 * time.Second))
		return ws.WriteControl(websocket.PongMessage, []byte(data), time.Now().Add(5*time.Second))
	})
	return &wsConn{ws: ws}
}

// ready reads the relay's access_ready.
func (w *wsConn) ready() (readyMsg, error) {
	for {
		typ, b, err := w.ws.ReadMessage()
		if err != nil {
			return readyMsg{}, fmt.Errorf("relay closed the stream before it was ready: %w", err)
		}
		if typ != websocket.TextMessage {
			continue
		}
		var m struct {
			Type string   `json:"type"`
			Data readyMsg `json:"data"`
		}
		if json.Unmarshal(b, &m) == nil && m.Type == "access_ready" {
			return m.Data, nil
		}
		if strings.Contains(string(b), "access_close") {
			return readyMsg{}, errors.New("relay closed the stream: " + string(b))
		}
	}
}

func (w *wsConn) Read(p []byte) (int, error) {
	w.rmu.Lock()
	defer w.rmu.Unlock()
	for len(w.buf) == 0 {
		typ, b, err := w.ws.ReadMessage()
		if err != nil {
			return 0, io.EOF
		}
		_ = w.ws.SetReadDeadline(time.Now().Add(90 * time.Second))
		switch typ {
		case websocket.BinaryMessage:
			w.buf = b
		case websocket.TextMessage:
			var m struct {
				Type string `json:"type"`
				Data struct {
					Reason string `json:"reason"`
				} `json:"data"`
			}
			if json.Unmarshal(b, &m) == nil && m.Type == "access_close" {
				w.wmu.Lock()
				w.closeRs = m.Data.Reason
				w.wmu.Unlock()
				return 0, io.EOF
			}
		}
	}
	n := copy(p, w.buf)
	w.buf = w.buf[n:]
	return n, nil
}

func (w *wsConn) Write(p []byte) (int, error) {
	w.wmu.Lock()
	defer w.wmu.Unlock()
	_ = w.ws.SetWriteDeadline(time.Now().Add(30 * time.Second))
	if err := w.ws.WriteMessage(websocket.BinaryMessage, p); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (w *wsConn) Close() error {
	w.once.Do(func() {
		w.wmu.Lock()
		_ = w.ws.SetWriteDeadline(time.Now().Add(5 * time.Second))
		_ = w.ws.WriteMessage(websocket.TextMessage, []byte(`{"type":"access_close","data":{"reason":"client"}}`))
		_ = w.ws.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(time.Second))
		w.wmu.Unlock()
		_ = w.ws.Close()
	})
	return nil
}

func (w *wsConn) reason() string {
	w.wmu.Lock()
	defer w.wmu.Unlock()
	return w.closeRs
}

func (w *wsConn) LocalAddr() net.Addr  { return dummyAddr{} }
func (w *wsConn) RemoteAddr() net.Addr { return dummyAddr{} }
func (w *wsConn) SetDeadline(t time.Time) error {
	_ = w.ws.SetReadDeadline(t)
	return w.ws.SetWriteDeadline(t)
}
func (w *wsConn) SetReadDeadline(t time.Time) error  { return w.ws.SetReadDeadline(t) }
func (w *wsConn) SetWriteDeadline(t time.Time) error { return w.ws.SetWriteDeadline(t) }

type dummyAddr struct{}

func (dummyAddr) Network() string { return "tunr-bridge" }
func (dummyAddr) String() string  { return "tunr-bridge" }

// NewClientSecret returns a random DSN password for local clients.
func NewClientSecret() string {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// DSN is the connection string for a postgres resource opened on addr.
func DSN(addr, secret string) string {
	return "postgresql://agent:" + secret + "@" + addr + "/db?sslmode=disable"
}
