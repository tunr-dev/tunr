package connector

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/gorilla/websocket"

	"github.com/tunr-dev/tunr/internal/bridge"
)

// Errors that stop Run instead of retrying: retrying cannot fix them.
var (
	ErrUnauthorized = errors.New("relay rejected the site token (rotated or site deleted?) — get a new one from the portal")
	ErrKeyMismatch  = errors.New("relay key does not match the pinned key — refusing to serve (possible interception, or the relay key was changed)")
	ErrReplaced     = errors.New("another connector is running for this site and replaced this one")
)

// RejectedError is a relay refusal of the hello (bad config, name conflict).
type RejectedError struct{ Code, Message string }

func (e *RejectedError) Error() string {
	return "relay refused the connector: " + e.Code + ": " + e.Message
}

// Streams handles stream traffic. P3 provides the real implementation; the
// default refuses every open.
type Streams interface {
	Ready(w bridge.WelcomeData)
	Open(ctx context.Context, o bridge.OpenData, send func(bridge.Message))
	Data(d bridge.DataPayload)
	Close(c bridge.CloseData)
	Kill(k bridge.KillData)
	CloseAll(reason string)
}

// Client is one connector process.
type Client struct {
	Cfg      *Config
	Token    string
	Version  string
	KillPath string  // local kill switch; "" disables it
	Streams  Streams // nil → refuse every open
	Logf     func(format string, args ...any)
	OnReady  func(w bridge.WelcomeData) // optional, for status output and tests
	// TLSFingerprint is announced in the hello when the connector can
	// terminate end-to-end TLS.
	TLSFingerprint string

	// Tunables (tests shorten them).
	HelloTimeout time.Duration
	ReadTimeout  time.Duration
	KillPoll     time.Duration
	MaxBackoff   time.Duration
}

func (c *Client) defaults() {
	if c.HelloTimeout == 0 {
		c.HelloTimeout = 15 * time.Second
	}
	if c.ReadTimeout == 0 {
		c.ReadTimeout = 90 * time.Second
	}
	if c.KillPoll == 0 {
		c.KillPoll = time.Second
	}
	if c.MaxBackoff == 0 {
		c.MaxBackoff = 60 * time.Second
	}
	if c.Logf == nil {
		c.Logf = func(string, ...any) {}
	}
	if c.Streams == nil {
		c.Streams = refuseAll{}
	}
}

// Killed reports whether the local kill switch is engaged.
func (c *Client) Killed() bool {
	if c.KillPath == "" {
		return false
	}
	_, err := os.Stat(c.KillPath)
	return err == nil
}

// Run keeps the connector attached until ctx ends or a non-retryable error.
// Backoff resets after a session that stayed up for a minute.
func (c *Client) Run(ctx context.Context) error {
	c.defaults()
	pin, err := ParseRelayKey(c.Cfg.RelayKey)
	if err != nil {
		return err
	}
	backoff := time.Second
	for {
		if c.Killed() {
			c.Logf("kill switch engaged (%s) — not connecting; run `tunr connect resume`", c.KillPath)
			if err := c.waitUnkilled(ctx); err != nil {
				return err
			}
			backoff = time.Second
		}
		start := time.Now()
		err := c.session(ctx, pin)
		switch {
		case ctx.Err() != nil:
			return ctx.Err()
		case errors.Is(err, ErrUnauthorized), errors.Is(err, ErrKeyMismatch), errors.Is(err, ErrReplaced):
			return err
		case errors.As(err, new(*RejectedError)):
			return err
		case errors.Is(err, errKilled):
			continue
		}
		if time.Since(start) > time.Minute {
			backoff = time.Second
		}
		delay := backoff + time.Duration(rand.Int63n(int64(backoff)/4+1))
		c.Logf("disconnected: %v — reconnecting in %s", err, delay.Round(100*time.Millisecond))
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
		if backoff *= 2; backoff > c.MaxBackoff {
			backoff = c.MaxBackoff
		}
	}
}

var errKilled = errors.New("kill switch engaged")

func (c *Client) waitUnkilled(ctx context.Context) error {
	t := time.NewTicker(c.KillPoll)
	defer t.Stop()
	for c.Killed() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
	c.Logf("kill switch released — connecting")
	return nil
}

func connectURL(relay string) string {
	u := strings.TrimRight(relay, "/")
	switch {
	case strings.HasPrefix(u, "https://"):
		u = "wss://" + strings.TrimPrefix(u, "https://")
	case strings.HasPrefix(u, "http://"):
		u = "ws://" + strings.TrimPrefix(u, "http://")
	}
	return u + "/bridge/connect"
}

func (c *Client) session(ctx context.Context, pin ed25519.PublicKey) error {
	hdr := http.Header{}
	hdr.Set("Authorization", "Bearer "+c.Token)
	hdr.Set("User-Agent", "tunr-connector/"+c.Version)
	dialer := websocket.Dialer{HandshakeTimeout: 15 * time.Second, Proxy: http.ProxyFromEnvironment}
	conn, resp, err := dialer.DialContext(ctx, connectURL(c.Cfg.Relay), hdr)
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		if resp != nil && resp.StatusCode == http.StatusUnauthorized {
			return ErrUnauthorized
		}
		return fmt.Errorf("dial: %w", err)
	}
	defer conn.Close()
	conn.SetReadLimit(8 << 20)

	_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	if err := conn.WriteJSON(bridge.NewMessage(bridge.MsgHello, bridge.HelloData{
		ConnectorVersion: c.Version, Resources: c.Cfg.Decls(), TLSCertSHA256: c.TLSFingerprint,
	})); err != nil {
		return fmt.Errorf("hello: %w", err)
	}

	_ = conn.SetReadDeadline(time.Now().Add(c.HelloTimeout))
	var m bridge.Message
	if err := conn.ReadJSON(&m); err != nil {
		return fmt.Errorf("welcome: %w", err)
	}
	if m.Type == bridge.MsgError {
		var e bridge.ErrorData
		_ = json.Unmarshal(m.Data, &e)
		return &RejectedError{Code: e.Code, Message: e.Message}
	}
	if m.Type != bridge.MsgWelcome {
		return fmt.Errorf("expected welcome, got %q", m.Type)
	}
	var w bridge.WelcomeData
	if err := json.Unmarshal(m.Data, &w); err != nil {
		return fmt.Errorf("welcome payload: %w", err)
	}
	got, err := ParseRelayKey(w.RelayKey)
	if err != nil || !got.Equal(pin) {
		return ErrKeyMismatch
	}
	c.Logf("connected: site %s (%s), %d resources", w.SiteName, w.SiteID, len(w.Resources))
	c.Streams.Ready(w)
	if c.OnReady != nil {
		c.OnReady(w)
	}

	sctx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer c.Streams.CloseAll("connector session ended")

	out := make(chan bridge.Message, 256)
	send := func(m bridge.Message) {
		select {
		case out <- m:
		case <-sctx.Done():
		}
	}
	errc := make(chan error, 3)

	// Single writer. On the way out, wait for it so the close frame is sent
	// before the deferred conn.Close (otherwise the relay sees 1006).
	writerDone := make(chan struct{})
	defer func() {
		cancel()
		<-writerDone
	}()
	go func() {
		defer close(writerDone)
		for {
			select {
			case <-sctx.Done():
				_ = conn.WriteControl(websocket.CloseMessage,
					websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(time.Second))
				return
			case m := <-out:
				_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
				if err := conn.WriteJSON(m); err != nil {
					errc <- fmt.Errorf("write: %w", err)
					return
				}
			}
		}
	}()

	// Kill switch watcher: engaging it closes every stream and the session.
	if c.KillPath != "" {
		go func() {
			t := time.NewTicker(c.KillPoll)
			defer t.Stop()
			for {
				select {
				case <-sctx.Done():
					return
				case <-t.C:
					if c.Killed() {
						c.Streams.CloseAll("killed")
						errc <- errKilled
						return
					}
				}
			}
		}()
	}

	// Reader.
	go func() {
		for {
			_ = conn.SetReadDeadline(time.Now().Add(c.ReadTimeout))
			var m bridge.Message
			if err := conn.ReadJSON(&m); err != nil {
				errc <- fmt.Errorf("read: %w", err)
				return
			}
			switch m.Type {
			case bridge.MsgPing:
				send(bridge.Message{Type: bridge.MsgPong})
			case bridge.MsgOpen:
				var o bridge.OpenData
				if json.Unmarshal(m.Data, &o) == nil && o.StreamID != "" {
					c.Streams.Open(sctx, o, send)
				}
			case bridge.MsgData:
				var d bridge.DataPayload
				if json.Unmarshal(m.Data, &d) == nil {
					c.Streams.Data(d)
				}
			case bridge.MsgClose:
				var cl bridge.CloseData
				if json.Unmarshal(m.Data, &cl) == nil {
					c.Streams.Close(cl)
				}
			case bridge.MsgKill:
				var k bridge.KillData
				if json.Unmarshal(m.Data, &k) == nil {
					c.Streams.Kill(k)
				}
			case bridge.MsgError:
				var e bridge.ErrorData
				_ = json.Unmarshal(m.Data, &e)
				if e.Code == "replaced" {
					errc <- ErrReplaced
				} else {
					errc <- fmt.Errorf("relay closed the session: %s: %s", e.Code, e.Message)
				}
				return
			default:
				// Unknown types are ignored (forward compatibility).
			}
		}
	}()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-errc:
		return err
	}
}

// refuseAll is the Streams used until stream handling exists (P3): every
// open is answered with a close, so nothing is ever dialled.
type refuseAll struct{}

func (refuseAll) Open(_ context.Context, o bridge.OpenData, send func(bridge.Message)) {
	send(bridge.NewMessage(bridge.MsgClose, bridge.CloseData{StreamID: o.StreamID, Reason: "unsupported"}))
}
func (refuseAll) Ready(bridge.WelcomeData) {}
func (refuseAll) Data(bridge.DataPayload)  {}
func (refuseAll) Close(bridge.CloseData)   {}
func (refuseAll) Kill(bridge.KillData)     {}
func (refuseAll) CloseAll(string)          {}
