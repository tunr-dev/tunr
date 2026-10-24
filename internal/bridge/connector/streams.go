package connector

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/tunr-dev/tunr/internal/auth"
	"github.com/tunr-dev/tunr/internal/bridge"
	"github.com/tunr-dev/tunr/internal/bridge/connector/mssqlq"
	"github.com/tunr-dev/tunr/internal/bridge/connector/pgproxy"
)

// streams.go — the connector's stream table (P3-T2).
//
// For every bridge_open the connector, independently of the relay:
//   - verifies the grant JWT with the pinned key (audience = this site),
//   - checks the JWT is bound to this stream (jti) and resource (res),
//     and has not been used before,
//   - resolves the resource ID to a name it announced and dials ONLY the
//     target in its local config for that name,
//   - refuses kinds it cannot enforce: raw tcp only for "rw" grants (TCP
//     can't be made read-only); postgres and mssql only for "ro" grants and
//     only through the connector's own session (pgproxy, mssqlq) — there is
//     no code path that pipes raw bytes to a database resource.
//
// Close reasons sent to the relay never include target addresses.

const (
	maxStreams     = 256
	maxPending     = 1 << 20
	dialTimeout    = 10 * time.Second
	streamReadSize = 32 << 10
)

// Manager implements Streams.
type Manager struct {
	Cfg     *Config
	Secrets auth.SecretStore // database credentials for postgres resources
	Pin     ed25519.PublicKey
	Logf    func(string, ...any)
	Dial    func(ctx context.Context, network, addr string) (net.Conn, error) // tests override
	Now     func() time.Time

	// TLSCert, when set, lets the connector terminate end-to-end TLS for
	// streams that ask for it (see e2e.go).
	TLSCert *tls.Certificate

	mu      sync.Mutex
	siteID  string
	resByID map[string]*Resource
	streams map[string]*stream
	seen    map[string]time.Time // used grant JWT IDs → exp, for replay refusal
}

// NewManager builds the stream table for cfg.
func NewManager(cfg *Config, logf func(string, ...any)) (*Manager, error) {
	pin, err := ParseRelayKey(cfg.RelayKey)
	if err != nil {
		return nil, err
	}
	if logf == nil {
		logf = func(string, ...any) {}
	}
	var d net.Dialer
	return &Manager{Cfg: cfg, Pin: pin, Logf: logf, Dial: d.DialContext, Now: time.Now,
		streams: map[string]*stream{}, seen: map[string]time.Time{}}, nil
}

type stream struct {
	id, grantID string
	send        func(bridge.Message)
	mu          sync.Mutex
	conn        net.Conn
	pending     [][]byte
	pendingN    int
	closed      bool
	cancel      context.CancelFunc
}

// Ready maps the relay's resource IDs to local resources (from the welcome).
func (m *Manager) Ready(w bridge.WelcomeData) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.siteID = w.SiteID
	m.resByID = map[string]*Resource{}
	for _, wr := range w.Resources {
		if r, ok := m.Cfg.Resource(wr.Name); ok {
			m.resByID[wr.ID] = r
		}
	}
}

func refuse(send func(bridge.Message), id, reason string) {
	send(bridge.NewMessage(bridge.MsgClose, bridge.CloseData{StreamID: id, Reason: reason}))
}

// Open handles bridge_open.
func (m *Manager) Open(ctx context.Context, o bridge.OpenData, send func(bridge.Message)) {
	now := m.Now()
	m.mu.Lock()
	siteID, res := m.siteID, m.resByID[o.ResourceID]
	m.mu.Unlock()

	claims, err := VerifyGrant(m.Pin, o.GrantJWT, siteID, now)
	if err != nil {
		m.Logf("refused stream %s: %v", o.StreamID, err)
		refuse(send, o.StreamID, "invalid_grant")
		return
	}
	if claims.ID != o.StreamID || claims.Resource != o.ResourceID {
		m.Logf("refused stream %s: grant bound to another stream/resource", o.StreamID)
		refuse(send, o.StreamID, "invalid_grant")
		return
	}
	if res == nil {
		m.Logf("refused stream %s: resource %s is not served here", o.StreamID, o.ResourceID)
		refuse(send, o.StreamID, "unknown_resource")
		return
	}
	switch {
	case res.Kind == "tcp" && claims.Mode != "rw":
		m.Logf("refused stream %s to %s: raw TCP cannot be read-only; needs an rw grant", o.StreamID, res.Name)
		refuse(send, o.StreamID, "mode_not_enforceable")
		return
	case res.IsDatabase() && claims.Mode != "ro":
		m.Logf("refused stream %s to %s: %s is read-only through the bridge", o.StreamID, res.Name, res.Kind)
		refuse(send, o.StreamID, "mode_not_allowed")
		return
	case res.IsDatabase() && res.Credential == "":
		m.Logf("refused stream %s to %s: no credential stored (tunr connect %s-role --resource %s)", o.StreamID, res.Name,
			map[string]string{"postgres": "pg", "mssql": "mssql"}[res.Kind], res.Name)
		refuse(send, o.StreamID, "no_credential")
		return
	case res.IsDatabase() && len(o.ClientSecretHash) != 64:
		m.Logf("refused stream %s to %s: no client secret (old tunr access?)", o.StreamID, res.Name)
		refuse(send, o.StreamID, "no_client_secret")
		return
	case res.Kind != "tcp" && !res.IsDatabase():
		refuse(send, o.StreamID, "kind_unsupported")
		return
	}

	m.mu.Lock()
	for jti, exp := range m.seen {
		if now.After(exp.Add(clockSkew)) {
			delete(m.seen, jti)
		}
	}
	_, replay := m.seen[claims.ID]
	_, dup := m.streams[o.StreamID]
	full := len(m.streams) >= maxStreams
	if !replay && !dup && !full {
		m.seen[claims.ID] = time.Unix(claims.Exp, 0)
	}
	if replay || dup || full {
		m.mu.Unlock()
		reason := "invalid_grant"
		if full {
			reason = "too_many_streams"
		}
		m.Logf("refused stream %s: replay=%v dup=%v full=%v", o.StreamID, replay, dup, full)
		refuse(send, o.StreamID, reason)
		return
	}
	sctx, cancel := context.WithCancel(ctx)
	st := &stream{id: o.StreamID, grantID: claims.GrantID, send: send, cancel: cancel}
	m.streams[o.StreamID] = st
	m.mu.Unlock()

	m.Logf("stream %s → %s (grant %s, agent %s, %s)", o.StreamID, res.Name, claims.GrantID, claims.Subject, claims.Mode)
	if o.E2E && m.TLSCert == nil {
		m.mu.Lock()
		delete(m.streams, o.StreamID)
		m.mu.Unlock()
		cancel()
		refuse(send, o.StreamID, "e2e_unavailable")
		return
	}
	go m.run(sctx, st, res, claims, o.ClientSecretHash, o.E2E)
}

// open returns the connection a stream's bytes flow into: the target itself
// for tcp, or one end of an in-memory pipe whose other end is a pgproxy
// (postgres) or mssqlq (mssql) session.
func (m *Manager) open(ctx context.Context, st *stream, res *Resource, claims *GrantClaims, clientHash string) (net.Conn, error) {
	if !res.IsDatabase() {
		dctx, cancel := context.WithTimeout(ctx, dialTimeout)
		defer cancel()
		return m.Dial(dctx, "tcp", res.Target)
	}
	if m.Secrets == nil {
		return nil, errors.New("no secret store")
	}
	raw, err := m.Secrets.Get(res.Credential)
	if err != nil {
		return nil, fmt.Errorf("credential %s: %w", res.Credential, err)
	}
	var cred struct{ User, Password string }
	if err := json.Unmarshal([]byte(raw), &cred); err != nil || cred.User == "" {
		return nil, fmt.Errorf("credential %s is not {user, password}", res.Credential)
	}
	agentSide, proxySide := net.Pipe()
	if res.Kind == "mssql" {
		lim := tighter(res.Limits, claims.Limits)
		cfg := mssqlq.Config{
			Upstream: res.Target, Database: res.Database, User: cred.User, Password: cred.Password,
			Encrypt: res.Encrypt, TrustServerCertificate: res.TrustServerCertificate, ClientSecretHash: clientHash,
			Limits:  mssqlq.Limits{MaxRows: lim.MaxRows, MaxBytes: lim.MaxBytes, QPS: lim.QPS},
			AppName: "tunr-bridge " + claims.Subject, Logf: m.Logf,
			Audit: func(a mssqlq.Audit) {
				st.send(bridge.NewMessage(bridge.MsgAudit, bridge.AuditData{
					StreamID: st.id, Command: a.Command, Tables: a.Tables, RowCount: a.RowCount,
					DurationMS: a.DurationMS, Blocked: a.Blocked, QueryText: a.QueryText,
				}))
			},
		}
		go func() {
			if err := mssqlq.Serve(ctx, proxySide, cfg); err != nil {
				m.Logf("stream %s: mssql: %v", st.id, err)
			}
		}()
		return agentSide, nil
	}
	var masks []pgproxy.Mask
	var maskKey []byte
	if len(res.Masks) > 0 {
		k, err := MaskKey(m.Secrets)
		if err != nil {
			return nil, err
		}
		maskKey = k
		for _, r := range res.Masks {
			masks = append(masks, pgproxy.Mask{Schema: r.Schema, Table: r.Table, Column: r.Column, Strategy: r.Strategy, KeepLast: r.KeepLast})
		}
	}
	cfg := pgproxy.Config{
		Upstream: res.Target, Database: res.Database, User: cred.User, Password: cred.Password,
		SSLMode: res.SSLMode, ClientSecretHash: clientHash, Limits: tighter(res.Limits, claims.Limits),
		AppName: "tunr-bridge " + claims.Subject, Logf: m.Logf, Masks: masks, MaskKey: maskKey,
		Audit: func(a pgproxy.Audit) {
			st.send(bridge.NewMessage(bridge.MsgAudit, bridge.AuditData{
				StreamID: st.id, Command: a.Command, Tables: a.Tables, RowCount: a.RowCount,
				DurationMS: a.DurationMS, Blocked: a.Blocked, QueryText: a.QueryText,
			}))
		},
	}
	go func() {
		if err := pgproxy.Serve(ctx, proxySide, cfg); err != nil {
			m.Logf("stream %s: pgproxy: %v", st.id, err)
		}
	}()
	return agentSide, nil
}

// MaskKeySecret names the secret that keys the "hash" masking strategy. It
// never leaves the site, so hashes can't be reversed by a dictionary elsewhere.
const MaskKeySecret = "mask-key"

// MaskKey returns the site's masking key, creating it on first use.
func MaskKey(secrets auth.SecretStore) ([]byte, error) {
	if secrets == nil {
		return nil, errors.New("no secret store")
	}
	if v, err := secrets.Get(MaskKeySecret); err == nil && len(v) >= 32 {
		return []byte(v), nil
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	k := hex.EncodeToString(b)
	if err := secrets.Set(MaskKeySecret, k); err != nil {
		return nil, fmt.Errorf("store mask key: %w", err)
	}
	return []byte(k), nil
}

// tighter combines the local resource limits with the grant's: per field,
// the smaller non-zero value wins.
func tighter(local, grant *Limits) pgproxy.Limits {
	var l, g Limits
	if local != nil {
		l = *local
	}
	if grant != nil {
		g = *grant
	}
	pick := func(a, b int64) int64 {
		switch {
		case a == 0:
			return b
		case b == 0:
			return a
		case a < b:
			return a
		}
		return b
	}
	return pgproxy.Limits{
		MaxRows:  int(pick(int64(l.MaxRows), int64(g.MaxRows))),
		MaxBytes: pick(l.MaxBytes, g.MaxBytes),
		QPS:      int(pick(int64(l.QPS), int64(g.QPS))),
	}
}

func (m *Manager) run(ctx context.Context, st *stream, res *Resource, claims *GrantClaims, clientHash string, e2e bool) {
	conn, err := m.open(ctx, st, res, claims, clientHash)
	if err == nil && e2e {
		conn = wrapTLS(conn, *m.TLSCert)
	}
	if err != nil {
		m.Logf("stream %s: open failed: %v", st.id, err)
		m.finish(st, "dial_failed", true)
		return
	}
	st.mu.Lock()
	if st.closed {
		st.mu.Unlock()
		_ = conn.Close()
		return
	}
	st.conn = conn
	for _, b := range st.pending {
		if _, err := conn.Write(b); err != nil {
			st.mu.Unlock()
			m.finish(st, "write_failed", true)
			return
		}
	}
	st.pending, st.pendingN = nil, 0
	st.mu.Unlock()

	buf := make([]byte, streamReadSize)
	for {
		n, err := conn.Read(buf)
		if n > 0 {
			st.send(bridge.NewMessage(bridge.MsgData, bridge.DataPayload{
				StreamID: st.id, PayloadB64: base64.StdEncoding.EncodeToString(buf[:n]),
			}))
		}
		if err != nil {
			reason := "eof"
			if !errors.Is(err, io.EOF) {
				reason = "read_failed"
			}
			m.finish(st, reason, true)
			return
		}
	}
}

// finish closes a stream once; notify sends bridge_close to the relay.
func (m *Manager) finish(st *stream, reason string, notify bool) {
	st.mu.Lock()
	if st.closed {
		st.mu.Unlock()
		return
	}
	st.closed = true
	if st.conn != nil {
		_ = st.conn.Close()
	}
	st.cancel()
	st.mu.Unlock()

	m.mu.Lock()
	delete(m.streams, st.id)
	m.mu.Unlock()
	if notify {
		refuse(st.send, st.id, reason)
	}
}

func (m *Manager) get(id string) *stream {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.streams[id]
}

// Data handles bridge_data from the relay (agent → resource).
func (m *Manager) Data(d bridge.DataPayload) {
	st := m.get(d.StreamID)
	if st == nil {
		return
	}
	b, err := base64.StdEncoding.DecodeString(d.PayloadB64)
	if err != nil {
		m.finish(st, "bad_data", true)
		return
	}
	st.mu.Lock()
	if st.conn == nil {
		if st.pendingN+len(b) > maxPending {
			st.mu.Unlock()
			m.finish(st, "buffer_full", true)
			return
		}
		st.pending = append(st.pending, b)
		st.pendingN += len(b)
		st.mu.Unlock()
		return
	}
	conn := st.conn
	st.mu.Unlock()
	_ = conn.SetWriteDeadline(time.Now().Add(30 * time.Second))
	if _, err := conn.Write(b); err != nil {
		m.finish(st, "write_failed", true)
	}
}

// Close handles bridge_close from the relay.
func (m *Manager) Close(c bridge.CloseData) {
	if st := m.get(c.StreamID); st != nil {
		m.finish(st, c.Reason, false)
	}
}

// Kill closes every stream of a grant ("*" = all) and tells the relay.
func (m *Manager) Kill(k bridge.KillData) {
	m.mu.Lock()
	var hit []*stream
	for _, st := range m.streams {
		if k.GrantID == "*" || st.grantID == k.GrantID {
			hit = append(hit, st)
		}
	}
	m.mu.Unlock()
	for _, st := range hit {
		m.finish(st, "killed", true)
	}
	if len(hit) > 0 {
		m.Logf("kill %s: closed %d streams", k.GrantID, len(hit))
	}
}

// CloseAll closes every stream without notifying (the session is ending).
func (m *Manager) CloseAll(reason string) {
	m.mu.Lock()
	all := make([]*stream, 0, len(m.streams))
	for _, st := range m.streams {
		all = append(all, st)
	}
	m.mu.Unlock()
	for _, st := range all {
		m.finish(st, reason, false)
	}
}

// Count returns the number of open streams.
func (m *Manager) Count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.streams)
}
