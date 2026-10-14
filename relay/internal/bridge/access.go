package bridge

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/gorilla/websocket"

	"github.com/tunr-dev/tunr/relay/internal/audit"
	"github.com/tunr-dev/tunr/relay/internal/auth"
	"github.com/tunr-dev/tunr/relay/internal/db"
	"github.com/tunr-dev/tunr/relay/internal/logger"
)

// access.go — GET /bridge/access (P3-T1): one WebSocket per agent-side TCP
// connection, bridged to the customer's connector under an approved grant.
//
// Every check happens BEFORE the upgrade, so a refused agent gets a plain
// HTTP status and a JSON {error, message} — `tunr access` turns the code into
// advice. Checks, in order: agent token (signature, typ, audience) → token
// not revoked and agent active (DB) → resource "<customer>/<name>" resolves
// for this vendor (active link) → approved, unexpired grant → site online.
//
// Agent leg framing: binary messages carry raw stream bytes; text messages
// carry JSON control ({type: access_ready | access_close}). The connector leg
// uses the bridge_* messages (base64 JSON), see connector_hub.go.

const (
	maxStreamsPerToken = 32
	agentChunk         = 32 << 10
	agentQueueLen      = 256
)

// Wire types on the agent leg.
const (
	MsgAccessReady = "access_ready"
	MsgAccessClose = "access_close"
)

// AccessReady is the first message after the upgrade.
type AccessReady struct {
	StreamID       string    `json:"stream_id"`
	GrantID        string    `json:"grant_id"`
	Mode           string    `json:"mode"`
	GrantExpiresAt time.Time `json:"grant_expires_at"`
}

var agentUpgrader = websocket.Upgrader{
	ReadBufferSize:  32 << 10,
	WriteBufferSize: 32 << 10,
	CheckOrigin:     func(r *http.Request) bool { return r.Header.Get("Origin") == "" },
}

// Access owns the live agent streams.
type Access struct {
	db     *db.DB
	jwt    *auth.JWTAuth
	signer *auth.GrantSigner
	hub    *ConnectorHub

	mu       sync.Mutex
	streams  map[string]*agentStream
	perToken map[string]int
}

// NewAccess wires the access path and installs itself as the hub's sink.
func NewAccess(d *db.DB, j *auth.JWTAuth, signer *auth.GrantSigner, hub *ConnectorHub) *Access {
	a := &Access{db: d, jwt: j, signer: signer, hub: hub,
		streams: map[string]*agentStream{}, perToken: map[string]int{}}
	hub.SetSink(a)
	return a
}

type agentStream struct {
	id, siteID, grantID, tokenID string
	agentID, vendorOrg, custOrg  string
	toAgent                      chan []byte // stream bytes for the agent
	done                         chan struct{}
	once                         sync.Once
	reason                       string
	fromConnector                bool // close initiated by the connector side
	up, down                     atomic.Int64
}

func (s *agentStream) close(reason string, fromConnector bool) {
	s.once.Do(func() {
		s.reason, s.fromConnector = reason, fromConnector
		close(s.done)
	})
}

func accessError(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": code, "message": msg})
}

// ServeAccess handles GET /bridge/access?resource=<customer>/<name>[&grant=grt_…].
func (a *Access) ServeAccess(w http.ResponseWriter, r *http.Request) {
	tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || tok == "" {
		accessError(w, http.StatusUnauthorized, "auth_required", "agent token required")
		return
	}
	claims, err := a.jwt.VerifyAgent(tok)
	if err != nil {
		accessError(w, http.StatusUnauthorized, "invalid_token", "invalid or expired agent token")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), dbTimeout)
	defer cancel()
	usable, err := a.db.AgentTokenUsable(ctx, claims.OrgID, claims.AgentID(), claims.TokenID())
	if err != nil {
		logger.Warn("bridge access: token check: %v", err)
		accessError(w, http.StatusServiceUnavailable, "unavailable", "try again")
		return
	}
	if !usable {
		accessError(w, http.StatusUnauthorized, "invalid_token", "agent token revoked or agent disabled")
		return
	}

	customer, name, ok := strings.Cut(r.URL.Query().Get("resource"), "/")
	if !ok || !slugRe.MatchString(customer) || !slugRe.MatchString(name) {
		accessError(w, http.StatusBadRequest, "bad_resource", `resource must be "<customer>/<name>", e.g. acme/erp-db`)
		return
	}
	res, err := a.db.ResolveVendorResource(ctx, claims.OrgID, customer, name)
	if errors.Is(err, db.ErrNotFound) {
		accessError(w, http.StatusNotFound, "unknown_resource", "no such resource, or the customer has not linked your org")
		return
	} else if err != nil {
		logger.Warn("bridge access: resolve: %v", err)
		accessError(w, http.StatusServiceUnavailable, "unavailable", "try again")
		return
	}
	grant, err := a.db.FindActiveGrant(ctx, claims.OrgID, claims.AgentID(), res.ID, r.URL.Query().Get("grant"))
	if errors.Is(err, db.ErrNotFound) {
		accessError(w, http.StatusForbidden, "no_grant", "no approved, unexpired grant for this agent and resource")
		return
	} else if err != nil {
		logger.Warn("bridge access: grant: %v", err)
		accessError(w, http.StatusServiceUnavailable, "unavailable", "try again")
		return
	}
	if !a.hub.Online(res.SiteID) {
		accessError(w, http.StatusServiceUnavailable, "site_offline", "the customer's connector is not connected")
		return
	}
	if !a.reserve(claims.TokenID()) {
		accessError(w, http.StatusTooManyRequests, "too_many_streams", "too many open streams for this token")
		return
	}
	defer a.release(claims.TokenID())

	// Short-lived proof for the connector. jti = stream ID, so the connector
	// can bind the token to exactly this stream.
	streamID := db.NewAccessSessionID()
	grantJWT, err := a.signer.Sign(auth.GrantClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject: claims.AgentID(), Audience: jwt.ClaimStrings{res.SiteID}, ID: streamID,
		},
		Resource: res.ID, GrantID: grant.ID, Mode: grant.Mode, Limits: effectiveLimits(res.Limits, grant.Limits),
	}, auth.MaxGrantJWTTTL)
	if err != nil {
		logger.Warn("bridge access: sign: %v", err)
		accessError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	if err := a.db.OpenAccessSession(ctx, streamID, grant.ID); err != nil {
		logger.Warn("bridge access: session row: %v", err)
		accessError(w, http.StatusServiceUnavailable, "unavailable", "try again")
		return
	}

	conn, err := agentUpgrader.Upgrade(w, r, nil)
	if err != nil {
		a.finishSession(streamID, "upgrade_failed", 0, 0)
		return
	}
	defer conn.Close()
	conn.SetReadLimit(1 << 20)

	st := &agentStream{
		id: streamID, siteID: res.SiteID, grantID: grant.ID, tokenID: claims.TokenID(),
		agentID: claims.AgentID(), vendorOrg: grant.VendorOrgID, custOrg: grant.CustomerOrgID,
		toAgent: make(chan []byte, agentQueueLen), done: make(chan struct{}),
	}
	a.mu.Lock()
	a.streams[streamID] = st
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		delete(a.streams, streamID)
		a.mu.Unlock()
	}()

	if err := a.hub.Send(res.SiteID, msg(MsgOpen, OpenData{StreamID: streamID, ResourceID: res.ID, GrantJWT: grantJWT})); err != nil {
		st.close("site_offline", false)
	}
	a.auditBoth(st, audit.ActionBridgeSessionOpened, map[string]any{
		"session_id": streamID, "grant_id": grant.ID, "agent_id": st.agentID, "resource_id": res.ID,
	})

	_ = conn.SetWriteDeadline(time.Now().Add(writeTimeout))
	_ = conn.WriteJSON(msg(MsgAccessReady, AccessReady{
		StreamID: streamID, GrantID: grant.ID, Mode: grant.Mode, GrantExpiresAt: *grant.ExpiresAt,
	}))

	go a.agentReader(conn, st)
	a.agentWriter(conn, st, time.Until(*grant.ExpiresAt))

	if !st.fromConnector {
		_ = a.hub.Send(st.siteID, msg(MsgClose, CloseData{StreamID: st.id, Reason: st.reason}))
	}
	a.finishSession(st.id, st.reason, st.up.Load(), st.down.Load())
	a.auditBoth(st, audit.ActionBridgeSessionClosed, map[string]any{
		"session_id": st.id, "grant_id": st.grantID, "reason": st.reason,
		"bytes_up": st.up.Load(), "bytes_down": st.down.Load(),
	})
}

func (a *Access) reserve(tokenID string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.perToken[tokenID] >= maxStreamsPerToken {
		return false
	}
	a.perToken[tokenID]++
	return true
}

func (a *Access) release(tokenID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.perToken[tokenID]--; a.perToken[tokenID] <= 0 {
		delete(a.perToken, tokenID)
	}
}

// agentReader: agent → connector.
func (a *Access) agentReader(conn *websocket.Conn, st *agentStream) {
	for {
		_ = conn.SetReadDeadline(time.Now().Add(readTimeout))
		typ, data, err := conn.ReadMessage()
		if err != nil {
			st.close("client", false)
			return
		}
		switch typ {
		case websocket.BinaryMessage:
			for len(data) > 0 {
				n := min(len(data), agentChunk)
				if err := a.hub.Send(st.siteID, msg(MsgData, DataPayload{
					StreamID: st.id, PayloadB64: base64.StdEncoding.EncodeToString(data[:n]),
				})); err != nil {
					st.close("site_offline", false)
					return
				}
				st.up.Add(int64(n))
				data = data[n:]
			}
		case websocket.TextMessage:
			var m Message
			if json.Unmarshal(data, &m) == nil && m.Type == MsgAccessClose {
				st.close("client", false)
				return
			}
		}
	}
}

// agentWriter: connector → agent, plus pings and the grant expiry deadline.
func (a *Access) agentWriter(conn *websocket.Conn, st *agentStream, untilExpiry time.Duration) {
	expiry := time.NewTimer(untilExpiry)
	defer expiry.Stop()
	ping := time.NewTicker(pingInterval)
	defer ping.Stop()
	for {
		select {
		case <-st.done:
			// Drain what the connector already sent before the close.
			for {
				select {
				case b := <-st.toAgent:
					_ = conn.SetWriteDeadline(time.Now().Add(writeTimeout))
					_ = conn.WriteMessage(websocket.BinaryMessage, b)
					continue
				default:
				}
				break
			}
			_ = conn.SetWriteDeadline(time.Now().Add(writeTimeout))
			_ = conn.WriteJSON(msg(MsgAccessClose, CloseData{StreamID: st.id, Reason: st.reason}))
			_ = conn.WriteControl(websocket.CloseMessage,
				websocket.FormatCloseMessage(websocket.CloseNormalClosure, truncate(st.reason, 120)),
				time.Now().Add(time.Second))
			return
		case b := <-st.toAgent:
			_ = conn.SetWriteDeadline(time.Now().Add(writeTimeout))
			if err := conn.WriteMessage(websocket.BinaryMessage, b); err != nil {
				st.close("client", false)
			}
		case <-ping.C:
			_ = conn.SetWriteDeadline(time.Now().Add(writeTimeout))
			if err := conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(writeTimeout)); err != nil {
				st.close("client", false)
			}
		case <-expiry.C:
			st.close("expired", false)
		}
	}
}

// FromConnector implements StreamSink. A connector may only speak for its
// own site's streams: anything else is dropped.
func (a *Access) FromConnector(siteID string, m Message) {
	var ref struct {
		StreamID string `json:"stream_id"`
	}
	if json.Unmarshal(m.Data, &ref) != nil {
		return
	}
	a.mu.Lock()
	st, ok := a.streams[ref.StreamID]
	a.mu.Unlock()
	if !ok || st.siteID != siteID {
		return
	}
	switch m.Type {
	case MsgData:
		var d DataPayload
		if json.Unmarshal(m.Data, &d) != nil {
			return
		}
		b, err := base64.StdEncoding.DecodeString(d.PayloadB64)
		if err != nil {
			return
		}
		select {
		case st.toAgent <- b:
			st.down.Add(int64(len(b)))
		default:
			// Never block the site's read loop on one slow agent.
			st.close("slow_consumer", false)
		}
	case MsgClose:
		var c CloseData
		_ = json.Unmarshal(m.Data, &c)
		reason := c.Reason
		if reason == "" {
			reason = "connector"
		}
		st.close(reason, true)
	case MsgAudit:
		// Per-query summaries arrive with pgproxy (P5).
	}
}

// SiteGone implements StreamSink.
func (a *Access) SiteGone(siteID string) {
	a.mu.Lock()
	var hit []*agentStream
	for _, st := range a.streams {
		if st.siteID == siteID {
			hit = append(hit, st)
		}
	}
	a.mu.Unlock()
	for _, st := range hit {
		st.close("site_offline", true)
	}
}

// KillGrant closes every live stream of a grant (P4 revocation uses it).
func (a *Access) KillGrant(grantID, reason string) int {
	a.mu.Lock()
	var hit []*agentStream
	for _, st := range a.streams {
		if st.grantID == grantID {
			hit = append(hit, st)
		}
	}
	a.mu.Unlock()
	for _, st := range hit {
		st.close(reason, false)
	}
	return len(hit)
}

func (a *Access) finishSession(id, reason string, up, down int64) {
	ctx, cancel := context.WithTimeout(context.Background(), dbTimeout)
	defer cancel()
	if err := a.db.CloseAccessSession(ctx, id, reason, up, down); err != nil {
		logger.Warn("bridge access: close session %s: %v", id, err)
	}
}

func (a *Access) auditBoth(st *agentStream, action string, detail map[string]any) {
	ctx, cancel := context.WithTimeout(context.Background(), dbTimeout)
	defer cancel()
	for _, org := range []string{st.custOrg, st.vendorOrg} {
		if err := a.db.AuditOrg(ctx, org, "", action, detail); err != nil {
			logger.Warn("bridge audit: %v", err)
		}
	}
}

// effectiveLimits takes, per field, the tighter of the resource's and the
// grant's limits (0 = unset). A grant can narrow a resource, never widen it.
func effectiveLimits(resource, grant json.RawMessage) *auth.GrantLimits {
	var r, g auth.GrantLimits
	_ = json.Unmarshal(resource, &r)
	_ = json.Unmarshal(grant, &g)
	tighter := func(a, b int64) int64 {
		switch {
		case a == 0:
			return b
		case b == 0:
			return a
		default:
			return min(a, b)
		}
	}
	out := auth.GrantLimits{
		MaxRows:  int(tighter(int64(r.MaxRows), int64(g.MaxRows))),
		MaxBytes: tighter(r.MaxBytes, g.MaxBytes),
		QPS:      int(tighter(int64(r.QPS), int64(g.QPS))),
	}
	if out == (auth.GrantLimits{}) {
		return nil
	}
	return &out
}
