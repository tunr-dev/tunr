package mssqlq

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"

	mssql "github.com/microsoft/go-mssqldb"

	"github.com/tunr-dev/tunr/internal/bridge/query"
)

// Limits are per-session ceilings (from the grant JWT, narrowed locally).
type Limits struct {
	MaxRows  int   // per query; 0 = unlimited (the response size cap still applies)
	MaxBytes int64 // per session, database → agent; 0 = unlimited
	QPS      int   // queries per second; 0 = unlimited
}

// Audit is one per-query summary, same shape as pgproxy's.
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
	Upstream               string // host:port from the connector's config
	Database               string
	User, Password         string // read-only login, from the secret store
	Encrypt                string // go-mssqldb encrypt: disable | false | true | strict ("" = false)
	TrustServerCertificate bool
	ClientSecretHash       string // hex sha256 of the agent's per-process secret
	Limits                 Limits
	AppName                string
	Audit                  func(Audit)
	Logf                   func(string, ...any)
	QueryTimeout           time.Duration // default 30s

	// Open replaces the SQL Server connection (tests).
	Open func(ctx context.Context) (*sql.DB, error)
}

const (
	maxAuditText    = 4096
	authTimeout     = 30 * time.Second
	defaultTimeout  = 30 * time.Second
	responseHeadway = 64 << 10 // room kept under query.MaxResponse for the envelope
)

// sessionSetup pins how the server parses statements (the classifier treats
// "…" as an identifier) and keeps agent queries from blocking the ERP: they
// give up on locks after 5s and lose any deadlock.
const sessionSetup = `SET QUOTED_IDENTIFIER ON; SET ANSI_NULLS ON; SET IMPLICIT_TRANSACTIONS OFF; ` +
	`SET XACT_ABORT ON; SET LOCK_TIMEOUT 5000; SET DEADLOCK_PRIORITY LOW;`

// DSN builds the go-mssqldb connection URL.
func DSN(cfg Config) (string, error) {
	host, port, err := net.SplitHostPort(cfg.Upstream)
	if err != nil {
		return "", err
	}
	enc := cfg.Encrypt
	if enc == "" {
		enc = "false" // go-mssqldb default: the login packet is encrypted when the server supports it
	}
	app := cfg.AppName
	if app == "" {
		app = "tunr-bridge"
	}
	q := url.Values{
		"database":     {cfg.Database},
		"app name":     {app},
		"encrypt":      {enc},
		"dial timeout": {"10"},
	}
	if cfg.TrustServerCertificate {
		q.Set("TrustServerCertificate", "true")
	}
	u := url.URL{Scheme: "sqlserver", User: url.UserPassword(cfg.User, cfg.Password),
		Host: net.JoinHostPort(host, port), RawQuery: q.Encode()}
	return u.String(), nil
}

func open(ctx context.Context, cfg Config) (*sql.DB, error) {
	if cfg.Open != nil {
		return cfg.Open(ctx)
	}
	dsn, err := DSN(cfg)
	if err != nil {
		return nil, err
	}
	c, err := mssql.NewConnector(dsn)
	if err != nil {
		return nil, err
	}
	return sql.OpenDB(c), nil
}

// Serve runs one session on client until either side ends.
func Serve(ctx context.Context, client net.Conn, cfg Config) error {
	defer client.Close()
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	if cfg.Audit == nil {
		cfg.Audit = func(Audit) {}
	}
	if cfg.QueryTimeout == 0 {
		cfg.QueryTimeout = defaultTimeout
	}
	qc := query.NewConn(client)

	_ = client.SetReadDeadline(time.Now().Add(authTimeout))
	var a query.Auth
	if err := qc.Read(&a, 4096); err != nil {
		return err
	}
	_ = client.SetReadDeadline(time.Time{})
	if !secretOK(a.Auth, cfg.ClientSecretHash) {
		_ = qc.Write(query.Hello{Error: &query.Error{Code: "auth_failed",
			Message: "tunr bridge: wrong client secret — use the endpoint and token printed by `tunr access`"}})
		return errors.New("client secret mismatch")
	}

	db, err := open(ctx, cfg)
	if err == nil {
		defer db.Close()
		db.SetMaxOpenConns(1)
		db.SetMaxIdleConns(1)
	}
	var conn *sql.Conn
	if err == nil {
		octx, cancel := context.WithTimeout(ctx, 15*time.Second)
		conn, err = db.Conn(octx)
		if err == nil {
			_, err = conn.ExecContext(octx, sessionSetup)
		}
		cancel()
	}
	if err != nil {
		cfg.Logf("mssql: upstream: %v", err)
		_ = qc.Write(query.Hello{Error: &query.Error{Code: "upstream",
			Message: "tunr bridge: the customer's database refused the connection"}})
		return err
	}
	defer conn.Close()
	if err := qc.Write(query.Hello{OK: true, Kind: "mssql"}); err != nil {
		return err
	}

	s := &session{cfg: cfg, conn: conn, bucket: float64(cfg.Limits.QPS), bucketAt: time.Now()}
	for {
		var req query.Request
		if err := qc.Read(&req, query.MaxRequest); err != nil {
			if errors.Is(err, query.ErrTooLarge) {
				_ = qc.Write(query.Response{Error: &query.Error{Code: "bad_request", Message: "statement too large"}})
			}
			return nil
		}
		resp, fatal := s.run(ctx, req)
		if err := qc.Write(resp); err != nil {
			return err
		}
		if fatal {
			return errors.New("session data limit reached")
		}
	}
}

func secretOK(secret, wantHash string) bool {
	if len(wantHash) != 64 {
		return false
	}
	sum := sha256.Sum256([]byte(secret))
	return subtle.ConstantTimeCompare([]byte(hex.EncodeToString(sum[:])), []byte(strings.ToLower(wantHash))) == 1
}

type session struct {
	cfg      Config
	conn     *sql.Conn
	bytes    int64
	bucket   float64
	bucketAt time.Time
}

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

func refusal(id, code, msg string) query.Response {
	return query.Response{ID: id, Error: &query.Error{Code: code, Message: msg}}
}

// run executes one request. fatal ends the session (data limit).
func (s *session) run(ctx context.Context, req query.Request) (query.Response, bool) {
	if strings.TrimSpace(req.SQL) == "" {
		return refusal(req.ID, "bad_request", "sql is required"), false
	}
	v := Classify(req.SQL)
	if !v.Allowed {
		s.cfg.Audit(Audit{Command: v.Command, Tables: v.Tables, Blocked: true, QueryText: truncate(req.SQL)})
		return refusal(req.ID, "blocked", "tunr bridge: "+v.Reason+
			". This connection is read-only and every statement is logged for the data owner."), false
	}
	if !s.allowQuery() {
		return refusal(req.ID, "rate_limited", fmt.Sprintf("tunr bridge: query rate limit (%d/s) for this grant", s.cfg.Limits.QPS)), false
	}
	maxRows := s.cfg.Limits.MaxRows
	if req.MaxRows > 0 && (maxRows == 0 || req.MaxRows < maxRows) {
		maxRows = req.MaxRows
	}

	start := time.Now()
	resp, fatal := s.execute(ctx, req.ID, req.SQL, maxRows)
	resp.DurationMS = int(time.Since(start).Milliseconds())
	s.cfg.Audit(Audit{Command: v.Command, Tables: v.Tables, RowCount: resp.RowCount,
		DurationMS: resp.DurationMS, QueryText: truncate(req.SQL)})
	return resp, fatal
}

func (s *session) execute(ctx context.Context, id, stmt string, maxRows int) (query.Response, bool) {
	qctx, cancel := context.WithTimeout(ctx, s.cfg.QueryTimeout)
	defer cancel()
	// Layer 2: whatever the statement does, it is rolled back.
	tx, err := s.conn.BeginTx(qctx, nil)
	if err != nil {
		return refusal(id, "db_error", err.Error()), false
	}
	defer func() { _ = tx.Rollback() }()
	rows, err := tx.QueryContext(qctx, stmt)
	if err != nil {
		return refusal(id, "db_error", dbMessage(err)), false
	}
	defer rows.Close()

	resp := query.Response{ID: id}
	size := 0
	for {
		types, err := rows.ColumnTypes()
		if err != nil {
			return refusal(id, "db_error", dbMessage(err)), false
		}
		rs := query.ResultSet{Columns: make([]query.Column, len(types)), Rows: [][]any{}}
		for i, t := range types {
			rs.Columns[i] = query.Column{Name: t.Name(), Type: t.DatabaseTypeName()}
		}
		vals := make([]any, len(types))
		ptrs := make([]any, len(types))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		for !resp.Truncated && rows.Next() {
			if maxRows > 0 && resp.RowCount >= maxRows {
				resp.Truncated = true
				resp.Notice = fmt.Sprintf("row limit (%d) for this grant reached; narrow the query (WHERE / TOP) or ask the data owner for a higher limit", maxRows)
				break
			}
			if err := rows.Scan(ptrs...); err != nil {
				return refusal(id, "db_error", dbMessage(err)), false
			}
			row := make([]any, len(vals))
			n := 0
			for i, v := range vals {
				row[i], n = convert(v, rs.Columns[i].Type), n+valueSize(v)
			}
			s.bytes += int64(n)
			if s.cfg.Limits.MaxBytes > 0 && s.bytes > s.cfg.Limits.MaxBytes {
				return refusal(id, "data_limit", fmt.Sprintf("tunr bridge: data limit (%d bytes) for this grant reached; session closed", s.cfg.Limits.MaxBytes)), true
			}
			if size += n + 8*len(row); size > query.MaxResponse-responseHeadway {
				resp.Truncated = true
				resp.Notice = "result larger than one response allows; narrow the query"
				break
			}
			rs.Rows = append(rs.Rows, row)
			resp.RowCount++
		}
		resp.ResultSets = append(resp.ResultSets, rs)
		if resp.Truncated || !rows.NextResultSet() {
			break
		}
	}
	if err := rows.Err(); err != nil && !resp.Truncated {
		return refusal(id, "db_error", dbMessage(err)), false
	}
	return resp, false
}

// convert turns a scanned value into something JSON shows faithfully.
func convert(v any, dbType string) any {
	switch x := v.(type) {
	case nil:
		return nil
	case []byte:
		switch dbType {
		case "DECIMAL", "NUMERIC", "MONEY", "SMALLMONEY":
			return string(x)
		case "UNIQUEIDENTIFIER":
			var u mssql.UniqueIdentifier
			if u.Scan(x) == nil {
				return u.String()
			}
		}
		return "0x" + strings.ToUpper(hex.EncodeToString(x))
	case time.Time:
		if dbType == "DATE" {
			return x.Format(time.DateOnly)
		}
		return x.Format(time.RFC3339Nano)
	case int64, float64, bool, string:
		return x
	default:
		return fmt.Sprint(x)
	}
}

func valueSize(v any) int {
	switch x := v.(type) {
	case []byte:
		return len(x)
	case string:
		return len(x)
	case nil:
		return 0
	}
	return 8
}

// dbMessage keeps SQL Server's message (the agent needs it to fix the query)
// but never the connection details.
func dbMessage(err error) string {
	var me mssql.Error
	if errors.As(err, &me) {
		return "mssql " + strconv.Itoa(int(me.Number)) + ": " + me.Message
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "query timed out (30s limit through the bridge)"
	}
	return err.Error()
}

func truncate(s string) string {
	if len(s) > maxAuditText {
		return s[:maxAuditText] + "…"
	}
	return s
}
