package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/tunr-dev/tunr/relay/internal/audit"
	"github.com/tunr-dev/tunr/relay/internal/auth"
	"github.com/tunr-dev/tunr/relay/internal/db"
	"github.com/tunr-dev/tunr/relay/internal/logger"
)

// connector_hub.go — GET /bridge/connect (P2-T1).
//
// One WebSocket per customer site. The connector authenticates with its site
// token in the Authorization header, BEFORE the upgrade: an unauthenticated
// caller never gets a socket. The hub keeps site_id → session in memory
// (single relay instance in the MVP; multi-instance routing is a later phase)
// and is the only way the rest of the bridge talks to a connector.
//
// Unlike the tunnel handler, every write goes through one writer goroutine —
// gorilla/websocket allows a single concurrent writer.

// Timeouts are vars so tests can shorten them.
var (
	helloTimeout = 15 * time.Second
	pingInterval = 30 * time.Second
	readTimeout  = 75 * time.Second // > 2 ping intervals
	writeTimeout = 10 * time.Second
	dbTimeout    = 5 * time.Second
)

const (
	maxResources    = 100
	sessionQueueLen = 256
	maxFrameBytes   = 8 << 20
)

// ErrSiteOffline is returned when no connector is attached for a site.
var ErrSiteOffline = errors.New("site offline")

var connectorUpgrader = websocket.Upgrader{
	ReadBufferSize:  32 << 10,
	WriteBufferSize: 32 << 10,
	// Connectors are not browsers. Any Origin header means a page is trying
	// to open this socket with ambient credentials; refuse.
	CheckOrigin: func(r *http.Request) bool { return r.Header.Get("Origin") == "" },
}

// StreamSink receives stream traffic coming back from connectors (P3 wires
// the access bridge in here).
type StreamSink interface {
	FromConnector(siteID string, m Message)
	// SiteGone: the site's connector session ended (or was replaced); every
	// stream it carried is dead.
	SiteGone(siteID string)
}

// ConnectorHub tracks live connector sessions.
type ConnectorHub struct {
	db       *db.DB
	relayKey string

	mu       sync.Mutex
	sessions map[string]*connSession
	sink     StreamSink
}

type connSession struct {
	siteID, orgID string
	tlsFP         string // connector's end-to-end TLS certificate sha256, "" if unsupported
	out           chan Message
	done          chan struct{}
	once          sync.Once
	reason        string
}

func (s *connSession) close(reason string) {
	s.once.Do(func() {
		s.reason = reason
		close(s.done)
	})
}

// NewConnectorHub builds a hub. relayKey is sent in every welcome.
func NewConnectorHub(d *db.DB, relayKey string) *ConnectorHub {
	return &ConnectorHub{db: d, relayKey: relayKey, sessions: map[string]*connSession{}}
}

// SetSink installs the receiver for stream messages.
func (h *ConnectorHub) SetSink(s StreamSink) {
	h.mu.Lock()
	h.sink = s
	h.mu.Unlock()
}

// Online reports whether a connector is attached for siteID.
func (h *ConnectorHub) Online(siteID string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	_, ok := h.sessions[siteID]
	return ok
}

// SiteTLS returns the attached connector's end-to-end TLS fingerprint.
func (h *ConnectorHub) SiteTLS(siteID string) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	if s, ok := h.sessions[siteID]; ok {
		return s.tlsFP
	}
	return ""
}

// Send queues m for siteID's connector. It never blocks: a connector that
// can't drain its queue is broken and gets disconnected.
func (h *ConnectorHub) Send(siteID string, m Message) error {
	h.mu.Lock()
	s, ok := h.sessions[siteID]
	h.mu.Unlock()
	if !ok {
		return ErrSiteOffline
	}
	select {
	case <-s.done:
		return ErrSiteOffline
	case s.out <- m:
		return nil
	default:
		s.close("send queue full")
		return ErrSiteOffline
	}
}

// dataHighWater: agent stream bytes may fill the send queue only this far, so
// control messages (open, close, kill) always have room.
const dataHighWater = sessionQueueLen * 3 / 4

// SendData queues stream bytes for siteID's connector with backpressure: when
// the queue is above dataHighWater it waits (slowing that agent's reads)
// until there is room, the stream ends (cancel) or wait runs out. A busy
// agent therefore slows down; it never disconnects the whole site.
func (h *ConnectorHub) SendData(siteID string, m Message, cancel <-chan struct{}, wait time.Duration) error {
	h.mu.Lock()
	s, ok := h.sessions[siteID]
	h.mu.Unlock()
	if !ok {
		return ErrSiteOffline
	}
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	for len(s.out) >= dataHighWater {
		select {
		case <-s.done:
			return ErrSiteOffline
		case <-cancel:
			return errStreamBackpressure
		case <-deadline.C:
			return errStreamBackpressure
		case <-time.After(5 * time.Millisecond):
		}
	}
	select {
	case <-s.done:
		return ErrSiteOffline
	case s.out <- m:
		return nil
	case <-cancel:
		return errStreamBackpressure
	case <-deadline.C:
		return errStreamBackpressure
	}
}

var errStreamBackpressure = errors.New("connector is not keeping up with this stream")

// Disconnect ends siteID's session (token rotated, site deleted or disabled).
func (h *ConnectorHub) Disconnect(siteID, reason string) {
	h.mu.Lock()
	s, ok := h.sessions[siteID]
	h.mu.Unlock()
	if ok {
		select {
		case s.out <- msg(MsgError, ErrorData{Code: "disconnected", Message: reason}):
		default:
		}
		s.close(reason)
	}
}

// ServeConnect handles GET /bridge/connect.
func (h *ConnectorHub) ServeConnect(w http.ResponseWriter, r *http.Request) {
	tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || !strings.HasPrefix(tok, auth.SiteTokenPrefix) {
		http.Error(w, "site token required", http.StatusUnauthorized)
		return
	}
	lctx, cancel := context.WithTimeout(r.Context(), dbTimeout)
	site, err := h.db.GetSiteByTokenHash(lctx, auth.HashSiteToken(tok))
	cancel()
	switch {
	case errors.Is(err, db.ErrNotFound):
		http.Error(w, "invalid site token", http.StatusUnauthorized)
		return
	case err != nil:
		logger.Warn("bridge connect: site lookup: %v", err)
		http.Error(w, "temporarily unavailable", http.StatusServiceUnavailable)
		return
	}

	conn, err := connectorUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return // Upgrade already wrote the HTTP error
	}
	defer conn.Close()
	conn.SetReadLimit(maxFrameBytes)

	hello, err := readHello(conn)
	if err != nil {
		closeWithError(conn, "bad_hello", err.Error())
		return
	}

	sctx, scancel := context.WithTimeout(context.Background(), dbTimeout)
	resources, err := h.db.SyncSiteResources(sctx, site.OrgID, site.ID, toDecls(hello.Resources))
	if err == nil {
		err = h.db.SetSiteOnline(sctx, site.ID, hello.ConnectorVersion, hello.TLSCertSHA256)
	}
	scancel()
	if err != nil {
		code := "internal"
		if errors.Is(err, db.ErrResourceOwnedByOtherSite) {
			code = "resource_conflict"
		} else {
			logger.Warn("bridge connect: site %s: %v", site.ID, err)
		}
		closeWithError(conn, code, err.Error())
		return
	}

	sess := &connSession{
		siteID: site.ID, orgID: site.OrgID, tlsFP: hello.TLSCertSHA256,
		out:  make(chan Message, sessionQueueLen),
		done: make(chan struct{}),
	}
	h.attach(sess)
	defer h.detach(sess)

	welcome := WelcomeData{SiteID: site.ID, SiteName: site.Name, RelayKey: h.relayKey}
	for _, res := range resources {
		welcome.Resources = append(welcome.Resources, WelcomeResource{Name: res.Name, ID: res.ID})
	}
	_ = conn.SetWriteDeadline(time.Now().Add(writeTimeout))
	if err := conn.WriteJSON(msg(MsgWelcome, welcome)); err != nil {
		return
	}
	h.audit(site, audit.ActionBridgeSiteConnected, map[string]any{
		"site_id": site.ID, "connector_version": hello.ConnectorVersion, "resources": len(resources),
	})
	logger.Info("bridge: site %s (%s) connected, %d resources", site.ID, site.Name, len(resources))

	go h.writeLoop(conn, sess)
	h.readLoop(conn, sess)

	h.audit(site, audit.ActionBridgeSiteDisconnected, map[string]any{"site_id": site.ID, "reason": sess.reason})
	logger.Info("bridge: site %s disconnected (%s)", site.ID, sess.reason)
}

func readHello(conn *websocket.Conn) (*HelloData, error) {
	_ = conn.SetReadDeadline(time.Now().Add(helloTimeout))
	var m Message
	if err := conn.ReadJSON(&m); err != nil {
		return nil, fmt.Errorf("read hello: %w", err)
	}
	if m.Type != MsgHello {
		return nil, fmt.Errorf("first message must be %s", MsgHello)
	}
	var hello HelloData
	if err := json.Unmarshal(m.Data, &hello); err != nil {
		return nil, fmt.Errorf("hello payload: %w", err)
	}
	if len(hello.Resources) > maxResources {
		return nil, fmt.Errorf("at most %d resources per site", maxResources)
	}
	if len(hello.ConnectorVersion) > 64 {
		return nil, errors.New("connector_version too long")
	}
	if hello.TLSCertSHA256 != "" && !hex64.MatchString(hello.TLSCertSHA256) {
		return nil, errors.New("tls_cert_sha256 must be 64 lowercase hex characters")
	}
	seen := map[string]bool{}
	for _, res := range hello.Resources {
		if !slugRe.MatchString(res.Name) || strings.Contains(res.Name, "--") {
			return nil, fmt.Errorf("invalid resource name %q", res.Name)
		}
		if res.Kind != "postgres" && res.Kind != "tcp" && res.Kind != "http" {
			return nil, fmt.Errorf("resource %s: unsupported kind %q", res.Name, res.Kind)
		}
		if seen[res.Name] {
			return nil, fmt.Errorf("duplicate resource %q", res.Name)
		}
		seen[res.Name] = true
	}
	return &hello, nil
}

func toDecls(in []ResourceDecl) []db.ResourceDecl {
	out := make([]db.ResourceDecl, len(in))
	for i, r := range in {
		out[i] = db.ResourceDecl{Name: r.Name, Kind: r.Kind}
	}
	return out
}

// attach registers sess, replacing (and closing) any previous session of the
// same site — the newest connector wins, the old one is told why.
func (h *ConnectorHub) attach(sess *connSession) {
	h.mu.Lock()
	old := h.sessions[sess.siteID]
	h.sessions[sess.siteID] = sess
	h.mu.Unlock()
	if old != nil {
		select {
		case old.out <- msg(MsgError, ErrorData{Code: "replaced", Message: "another connector connected for this site"}):
		default:
		}
		old.close("replaced by a newer connection")
		h.siteGone(sess.siteID)
	}
}

func (h *ConnectorHub) siteGone(siteID string) {
	h.mu.Lock()
	sink := h.sink
	h.mu.Unlock()
	if sink != nil {
		sink.SiteGone(siteID)
	}
}

// detach removes sess if it is still current and marks the site offline.
func (h *ConnectorHub) detach(sess *connSession) {
	sess.close("connection closed")
	h.mu.Lock()
	current := h.sessions[sess.siteID] == sess
	if current {
		delete(h.sessions, sess.siteID)
	}
	h.mu.Unlock()
	if current {
		h.siteGone(sess.siteID)
		ctx, cancel := context.WithTimeout(context.Background(), dbTimeout)
		defer cancel()
		if err := h.db.SetSiteOffline(ctx, sess.siteID); err != nil {
			logger.Warn("bridge: mark site %s offline: %v", sess.siteID, err)
		}
	}
}

func (h *ConnectorHub) readLoop(conn *websocket.Conn, sess *connSession) {
	defer sess.close("read loop ended")
	for {
		_ = conn.SetReadDeadline(time.Now().Add(readTimeout))
		var m Message
		if err := conn.ReadJSON(&m); err != nil {
			sess.close("read: " + err.Error())
			return
		}
		switch m.Type {
		case MsgPong:
		case MsgData, MsgClose, MsgAudit:
			h.mu.Lock()
			sink := h.sink
			h.mu.Unlock()
			if sink != nil {
				sink.FromConnector(sess.siteID, m)
			}
		default:
			// Unknown types are ignored (forward compatibility).
		}
		select {
		case <-sess.done:
			return
		default:
		}
	}
}

func (h *ConnectorHub) writeLoop(conn *websocket.Conn, sess *connSession) {
	ticker := time.NewTicker(pingInterval)
	defer ticker.Stop()
	write := func(m Message) bool {
		_ = conn.SetWriteDeadline(time.Now().Add(writeTimeout))
		return conn.WriteJSON(m) == nil
	}
	for {
		select {
		case <-sess.done:
			// Flush a pending error (replaced/disconnected) before closing.
			select {
			case m := <-sess.out:
				if m.Type == MsgError {
					write(m)
				}
			default:
			}
			_ = conn.WriteControl(websocket.CloseMessage,
				websocket.FormatCloseMessage(websocket.CloseNormalClosure, truncate(sess.reason, 120)),
				time.Now().Add(time.Second))
			_ = conn.Close()
			return
		case m := <-sess.out:
			if !write(m) {
				sess.close("write failed")
			}
		case <-ticker.C:
			if !write(Message{Type: MsgPing}) {
				sess.close("ping failed")
			}
		}
	}
}

func (h *ConnectorHub) audit(site *db.Site, action string, detail map[string]any) {
	ctx, cancel := context.WithTimeout(context.Background(), dbTimeout)
	defer cancel()
	if err := h.db.AuditOrg(ctx, site.OrgID, "", action, detail); err != nil {
		logger.Warn("bridge audit: %v", err)
	}
}

func closeWithError(conn *websocket.Conn, code, message string) {
	_ = conn.SetWriteDeadline(time.Now().Add(3 * time.Second))
	_ = conn.WriteJSON(msg(MsgError, ErrorData{Code: code, Message: message}))
	_ = conn.WriteControl(websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.ClosePolicyViolation, truncate(code, 120)),
		time.Now().Add(time.Second))
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
