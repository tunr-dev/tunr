package pgproxy

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgproto3"
)

// proxy.go — one agent stream ↔ one upstream session.
//
// Toward the agent the proxy is a PostgreSQL server: it asks for a cleartext
// password and accepts only the per-process secret `tunr access` put in the
// agent's DSN (its SHA-256 arrives with the grant, from the relay). The agent
// never sees — and can't choose — the database credentials: toward the
// customer's database the proxy logs in itself with the read-only role from
// the connector's secret store, forcing default_transaction_read_only=on.
//
// Audit unit is the "query cycle" (everything up to ReadyForQuery): command,
// tables, rows, duration, blocked or not.

// Limits are per-session ceilings (from the grant JWT, narrowed locally).
type Limits struct {
	MaxRows  int   // per query cycle; 0 = unlimited
	MaxBytes int64 // per session, server → agent; 0 = unlimited
	QPS      int   // queries per second; 0 = unlimited
}

// Audit is one per-query summary.
type Audit struct {
	Command    string
	Tables     []string
	RowCount   int
	DurationMS int
	Blocked    bool
	QueryText  string
}

// Config is everything a session needs.
type Config struct {
	Upstream         string // host:port from the connector's config
	Database         string
	User, Password   string // read-only role, from the secret store
	SSLMode          string // upstream sslmode (default "prefer")
	ClientSecretHash string // hex sha256 of the agent-side DSN password
	Limits           Limits
	AppName          string
	Audit            func(Audit)
	Logf             func(string, ...any)
	ConnectTimeout   time.Duration
}

const maxAuditText = 4096

// Serve runs one session on client until either side ends.
func Serve(ctx context.Context, client net.Conn, cfg Config) error {
	defer client.Close()
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	if cfg.Audit == nil {
		cfg.Audit = func(Audit) {}
	}
	be := pgproto3.NewBackend(client, client)

	startup, err := receiveStartup(client, be)
	if err != nil {
		return err
	}
	if err := authenticateClient(be, cfg.ClientSecretHash); err != nil {
		return err
	}

	hj, err := connectUpstream(ctx, cfg)
	if err != nil {
		cfg.Logf("pgproxy: upstream: %v", err)
		fatal(be, "08006", "tunr bridge: the customer's database refused the connection")
		return err
	}
	defer hj.Conn.Close()
	cfg.Logf("pgproxy: session for client user %q", startup.Parameters["user"])

	// Finish the client's startup with the upstream's parameters but our own
	// key data: cancel requests are not forwarded.
	be.Send(&pgproto3.AuthenticationOk{})
	for k, v := range hj.ParameterStatuses {
		be.Send(&pgproto3.ParameterStatus{Name: k, Value: v})
	}
	key := make([]byte, 4)
	_, _ = rand.Read(key)
	be.Send(&pgproto3.BackendKeyData{ProcessID: 0, SecretKey: key})
	be.Send(&pgproto3.ReadyForQuery{TxStatus: 'I'})
	if err := be.Flush(); err != nil {
		return err
	}

	s := &session{cfg: cfg, be: be, fe: hj.Frontend, txStatus: 'I',
		stmts: map[string]Verdict{}, portals: map[string]string{}, bucketAt: time.Now()}
	errc := make(chan error, 2)
	go func() { errc <- s.fromClient() }()
	go func() { errc <- s.fromServer() }()
	err = <-errc
	_ = client.Close()
	_ = hj.Conn.Close()
	if errors.Is(err, errTerminated) {
		return nil
	}
	return err
}

var errTerminated = errors.New("client terminated")

func receiveStartup(client net.Conn, be *pgproto3.Backend) (*pgproto3.StartupMessage, error) {
	_ = client.SetReadDeadline(time.Now().Add(30 * time.Second))
	defer func() { _ = client.SetReadDeadline(time.Time{}) }()
	for i := 0; i < 4; i++ {
		msg, err := be.ReceiveStartupMessage()
		if err != nil {
			return nil, err
		}
		switch m := msg.(type) {
		case *pgproto3.SSLRequest, *pgproto3.GSSEncRequest:
			// The bridge stream is already encrypted to the relay; no TLS here.
			if _, err := client.Write([]byte{'N'}); err != nil {
				return nil, err
			}
		case *pgproto3.CancelRequest:
			return nil, errors.New("cancel requests are not supported through the bridge")
		case *pgproto3.StartupMessage:
			return m, nil
		default:
			return nil, fmt.Errorf("unexpected startup message %T", msg)
		}
	}
	return nil, errors.New("too many startup negotiation messages")
}

func authenticateClient(be *pgproto3.Backend, wantHash string) error {
	if len(wantHash) != 64 {
		fatal(be, "28000", "tunr bridge: this stream has no client secret; use the DSN printed by `tunr access`")
		return errors.New("no client secret hash")
	}
	be.Send(&pgproto3.AuthenticationCleartextPassword{})
	if err := be.Flush(); err != nil {
		return err
	}
	if err := be.SetAuthType(pgproto3.AuthTypeCleartextPassword); err != nil {
		return err
	}
	msg, err := be.Receive()
	if err != nil {
		return err
	}
	pw, ok := msg.(*pgproto3.PasswordMessage)
	sum := sha256.Sum256([]byte(""))
	if ok {
		sum = sha256.Sum256([]byte(pw.Password))
	}
	if !ok || subtle.ConstantTimeCompare([]byte(hex.EncodeToString(sum[:])), []byte(strings.ToLower(wantHash))) != 1 {
		fatal(be, "28P01", "tunr bridge: password authentication failed — use the DSN printed by `tunr access`")
		return errors.New("client password mismatch")
	}
	return nil
}

func connectUpstream(ctx context.Context, cfg Config) (*pgconn.HijackedConn, error) {
	host, port, err := net.SplitHostPort(cfg.Upstream)
	if err != nil {
		return nil, err
	}
	sslmode := cfg.SSLMode
	if sslmode == "" {
		sslmode = "prefer"
	}
	u := url.URL{Scheme: "postgres", User: url.UserPassword(cfg.User, cfg.Password),
		Host: net.JoinHostPort(host, port), Path: "/" + cfg.Database,
		RawQuery: url.Values{"sslmode": {sslmode}}.Encode()}
	pc, err := pgconn.ParseConfig(u.String())
	if err != nil {
		return nil, err
	}
	pc.Fallbacks = nil
	timeout := cfg.ConnectTimeout
	if timeout == 0 {
		timeout = 10 * time.Second
	}
	pc.ConnectTimeout = timeout
	app := cfg.AppName
	if app == "" {
		app = "tunr-bridge"
	}
	// Layer 2: the session itself is read-only, whatever the role allows.
	pc.RuntimeParams = map[string]string{
		"default_transaction_read_only":       "on",
		"statement_timeout":                   "30s",
		"idle_in_transaction_session_timeout": "60s",
		"application_name":                    app,
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	conn, err := pgconn.ConnectConfig(cctx, pc)
	if err != nil {
		return nil, err
	}
	return conn.Hijack()
}

func fatal(be *pgproto3.Backend, code, msg string) {
	be.Send(&pgproto3.ErrorResponse{Severity: "FATAL", SeverityUnlocalized: "FATAL", Code: code, Message: msg})
	_ = be.Flush()
}

// ── session ──────────────────────────────────────────────────────────────────

type cycle struct {
	start    time.Time
	commands []string
	tables   []string
	text     []string
	rows     int
	active   bool
}

type session struct {
	cfg Config
	be  *pgproto3.Backend
	fe  *pgproto3.Frontend

	beMu     sync.Mutex // the client side is written by both goroutines
	mu       sync.Mutex // cycle, txStatus, limits state
	cur      cycle
	txStatus byte
	exceeded bool  // current cycle hit max_rows; drop rows until ReadyForQuery
	bytes    int64 // server → client, session total

	// client goroutine only
	skipUntilSync bool
	stmts         map[string]Verdict // prepared statement name → verdict
	portals       map[string]string  // portal → statement
	bucket        float64
	bucketAt      time.Time
}

func (s *session) toClient(flush bool, msgs ...pgproto3.BackendMessage) error {
	s.beMu.Lock()
	defer s.beMu.Unlock()
	for _, m := range msgs {
		s.be.Send(m)
	}
	if flush {
		return s.be.Flush()
	}
	return nil
}

func (s *session) blockErr(reason string) *pgproto3.ErrorResponse {
	return &pgproto3.ErrorResponse{Severity: "ERROR", SeverityUnlocalized: "ERROR", Code: "42501",
		Message: "tunr bridge: " + reason, Hint: "This connection is read-only and every statement is logged for the data owner."}
}

func (s *session) currentTx() byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.txStatus
}

func (s *session) auditBlocked(v Verdict, sql string) {
	s.cfg.Audit(Audit{Command: v.Command, Tables: v.Tables, Blocked: true, QueryText: truncate(sql)})
}

// allowQuery applies the QPS token bucket.
func (s *session) allowQuery() bool {
	if s.cfg.Limits.QPS <= 0 {
		return true
	}
	now := time.Now()
	s.bucket += now.Sub(s.bucketAt).Seconds() * float64(s.cfg.Limits.QPS)
	if max := float64(s.cfg.Limits.QPS); s.bucket > max {
		s.bucket = max
	}
	s.bucketAt = now
	if s.bucket < 1 {
		return false
	}
	s.bucket--
	return true
}

func (s *session) beginOrJoin(v Verdict, sql string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.cur.active {
		s.cur = cycle{start: time.Now(), active: true}
	}
	s.cur.commands = append(s.cur.commands, v.Command)
	s.cur.tables = append(s.cur.tables, v.Tables...)
	s.cur.text = append(s.cur.text, sql)
}

func (s *session) fromClient() error {
	for {
		msg, err := s.be.Receive()
		if err != nil {
			return err
		}
		switch m := msg.(type) {
		case *pgproto3.Query:
			v := Classify(m.String)
			if !v.Allowed {
				s.auditBlocked(v, m.String)
				if err := s.toClient(true, s.blockErr(v.Reason), &pgproto3.ReadyForQuery{TxStatus: s.currentTx()}); err != nil {
					return err
				}
				continue
			}
			if !s.allowQuery() {
				if err := s.toClient(true, &pgproto3.ErrorResponse{Severity: "ERROR", Code: "53400",
					Message: fmt.Sprintf("tunr bridge: query rate limit (%d/s) for this grant", s.cfg.Limits.QPS)},
					&pgproto3.ReadyForQuery{TxStatus: s.currentTx()}); err != nil {
					return err
				}
				continue
			}
			s.beginOrJoin(v, m.String)
			s.fe.Send(m)
			if err := s.fe.Flush(); err != nil {
				return err
			}

		case *pgproto3.Parse:
			if s.skipUntilSync {
				continue
			}
			v := Classify(m.Query)
			if !v.Allowed {
				s.auditBlocked(v, m.Query)
				s.skipUntilSync = true
				if err := s.toClient(true, s.blockErr(v.Reason)); err != nil {
					return err
				}
				continue
			}
			s.stmts[m.Name] = v
			s.fe.Send(m)

		case *pgproto3.Bind:
			if s.skipUntilSync {
				continue
			}
			s.portals[m.DestinationPortal] = m.PreparedStatement
			s.fe.Send(m)

		case *pgproto3.Execute:
			if s.skipUntilSync {
				continue
			}
			if !s.allowQuery() {
				s.skipUntilSync = true
				if err := s.toClient(true, &pgproto3.ErrorResponse{Severity: "ERROR", Code: "53400",
					Message: fmt.Sprintf("tunr bridge: query rate limit (%d/s) for this grant", s.cfg.Limits.QPS)}); err != nil {
					return err
				}
				continue
			}
			v := s.stmts[s.portals[m.Portal]]
			if v.Command == "" {
				v.Command = "EXECUTE"
			}
			s.beginOrJoin(v, "")
			s.fe.Send(m)

		case *pgproto3.Describe, *pgproto3.Close:
			if s.skipUntilSync {
				continue
			}
			s.fe.Send(m)

		case *pgproto3.Flush:
			if s.skipUntilSync {
				continue
			}
			s.fe.Send(m)
			if err := s.fe.Flush(); err != nil {
				return err
			}

		case *pgproto3.Sync:
			if s.skipUntilSync {
				s.skipUntilSync = false
				if err := s.toClient(true, &pgproto3.ReadyForQuery{TxStatus: s.currentTx()}); err != nil {
					return err
				}
				continue
			}
			s.fe.Send(m)
			if err := s.fe.Flush(); err != nil {
				return err
			}

		case *pgproto3.Terminate:
			s.fe.Send(m)
			_ = s.fe.Flush()
			return errTerminated

		case *pgproto3.FunctionCall:
			// Fast-path function calls bypass SQL entirely.
			s.auditBlocked(Verdict{Command: "FUNCTION_CALL"}, "")
			if err := s.toClient(true, s.blockErr("function calls are not allowed"),
				&pgproto3.ReadyForQuery{TxStatus: s.currentTx()}); err != nil {
				return err
			}

		case *pgproto3.CopyData, *pgproto3.CopyDone, *pgproto3.CopyFail:
			// COPY is refused by the classifier, so there is no COPY to feed.
			continue

		default:
			return fmt.Errorf("pgproxy: unsupported client message %T", msg)
		}
	}
}

func (s *session) fromServer() error {
	sinceFlush := 0
	for {
		msg, err := s.fe.Receive()
		if err != nil {
			return err
		}
		switch m := msg.(type) {
		case *pgproto3.DataRow:
			size := 0
			for _, v := range m.Values {
				size += len(v)
			}
			s.mu.Lock()
			s.cur.rows++
			s.bytes += int64(size)
			rows, total, exceeded := s.cur.rows, s.bytes, s.exceeded
			overRows := s.cfg.Limits.MaxRows > 0 && rows > s.cfg.Limits.MaxRows && !exceeded
			if overRows {
				s.exceeded = true
			}
			s.mu.Unlock()
			if s.cfg.Limits.MaxBytes > 0 && total > s.cfg.Limits.MaxBytes {
				_ = s.toClient(true, &pgproto3.ErrorResponse{Severity: "FATAL", Code: "54000",
					Message: fmt.Sprintf("tunr bridge: data limit (%d bytes) for this grant reached; session closed", s.cfg.Limits.MaxBytes)})
				s.finishCycle(true)
				return errors.New("max_bytes exceeded")
			}
			if overRows {
				// No upstream CancelRequest: the protocol can't say *which*
				// query to cancel, so a cancel racing the end of this query
				// would kill the agent's next one. The rest of the result is
				// drained instead; statement_timeout (30s) bounds the cost.
				if err := s.toClient(true, &pgproto3.ErrorResponse{Severity: "ERROR", Code: "54000",
					Message: fmt.Sprintf("tunr bridge: row limit (%d) for this grant exceeded; the query was stopped", s.cfg.Limits.MaxRows),
					Hint:    "Narrow the query (WHERE / LIMIT) or ask the data owner for a higher limit."}); err != nil {
					return err
				}
				continue
			}
			if exceeded {
				continue
			}
			if err := s.toClient(false, m); err != nil {
				return err
			}
			if sinceFlush++; sinceFlush >= 64 {
				sinceFlush = 0
				if err := s.toClient(true); err != nil {
					return err
				}
			}

		case *pgproto3.ReadyForQuery:
			s.mu.Lock()
			s.txStatus = m.TxStatus
			s.exceeded = false
			s.mu.Unlock()
			s.finishCycle(false)
			sinceFlush = 0
			if err := s.toClient(true, m); err != nil {
				return err
			}

		default:
			s.mu.Lock()
			exceeded := s.exceeded
			s.mu.Unlock()
			if exceeded {
				// Swallow the rest of the stopped query (rows, the cancel's
				// own error, CommandComplete) until ReadyForQuery.
				switch msg.(type) {
				case *pgproto3.CommandComplete, *pgproto3.ErrorResponse, *pgproto3.PortalSuspended:
					continue
				}
			}
			sinceFlush = 0
			if err := s.toClient(true, m); err != nil {
				return err
			}
		}
	}
}

func (s *session) finishCycle(force bool) {
	s.mu.Lock()
	c := s.cur
	s.cur = cycle{}
	s.mu.Unlock()
	if !c.active && !force {
		return
	}
	cmd := "UNKNOWN"
	if len(c.commands) > 0 {
		cmd = c.commands[0]
	}
	s.cfg.Audit(Audit{Command: cmd, Tables: dedupe(c.tables), RowCount: c.rows,
		DurationMS: int(time.Since(c.start).Milliseconds()), QueryText: truncate(strings.Join(nonEmpty(c.text), ";\n"))})
}

func truncate(s string) string {
	if len(s) > maxAuditText {
		return s[:maxAuditText] + "…"
	}
	return s
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

func nonEmpty(in []string) []string {
	var out []string
	for _, s := range in {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

// HashSecret is how `tunr access` and the relay agree on the client secret.
func HashSecret(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}
