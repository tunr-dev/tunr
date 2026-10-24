package mssqlq

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"database/sql/driver"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tunr-dev/tunr/internal/bridge/access"
	"github.com/tunr-dev/tunr/internal/bridge/query"
)

// ── a fake database/sql driver ───────────────────────────────────────────────

type fakeDB struct {
	mu        sync.Mutex
	execs     []string
	queries   []string
	rollbacks int
	commits   int
	rows      int // rows returned per query
}

func (f *fakeDB) Open(string) (driver.Conn, error) { return &fakeConn{db: f}, nil }

type fakeConn struct{ db *fakeDB }

func (c *fakeConn) Prepare(string) (driver.Stmt, error) { return nil, errors.New("no prepare") }
func (c *fakeConn) Close() error                        { return nil }
func (c *fakeConn) Begin() (driver.Tx, error)           { return &fakeTx{db: c.db}, nil }
func (c *fakeConn) ExecContext(_ context.Context, q string, _ []driver.NamedValue) (driver.Result, error) {
	c.db.mu.Lock()
	c.db.execs = append(c.db.execs, q)
	c.db.mu.Unlock()
	return driver.RowsAffected(0), nil
}
func (c *fakeConn) QueryContext(_ context.Context, q string, _ []driver.NamedValue) (driver.Rows, error) {
	c.db.mu.Lock()
	c.db.queries = append(c.db.queries, q)
	n := c.db.rows
	c.db.mu.Unlock()
	if strings.Contains(q, "boom") {
		return nil, errors.New("Invalid column name 'boom'.")
	}
	return &fakeRows{n: n, sets: 1 + strings.Count(q, ";")}, nil
}

type fakeTx struct{ db *fakeDB }

func (t *fakeTx) Commit() error { t.db.mu.Lock(); t.db.commits++; t.db.mu.Unlock(); return nil }
func (t *fakeTx) Rollback() error {
	t.db.mu.Lock()
	t.db.rollbacks++
	t.db.mu.Unlock()
	return nil
}

type fakeRows struct {
	n, i, sets, set int
}

func (r *fakeRows) Columns() []string { return []string{"ItemCode", "Qty", "Price", "Id"} }
func (r *fakeRows) Close() error      { return nil }
func (r *fakeRows) Next(dest []driver.Value) error {
	if r.i >= r.n {
		return io.EOF
	}
	r.i++
	dest[0] = "ITEM-" + string(rune('A'+r.i-1))
	dest[1] = int64(r.i)
	dest[2] = []byte("12.50")
	dest[3] = []byte{0x78, 0x56, 0x34, 0x12, 0x34, 0x12, 0x34, 0x12, 0x12, 0x34, 0x56, 0x78, 0x9a, 0xbc, 0xde, 0xf0}
	return nil
}
func (r *fakeRows) ColumnTypeDatabaseTypeName(i int) string {
	return []string{"NVARCHAR", "BIGINT", "DECIMAL", "UNIQUEIDENTIFIER"}[i]
}
func (r *fakeRows) HasNextResultSet() bool { return r.set+1 < r.sets }
func (r *fakeRows) NextResultSet() error {
	if !r.HasNextResultSet() {
		return io.EOF
	}
	r.set++
	r.i = 0
	return nil
}

var registerOnce sync.Once
var current *fakeDB

func init() {
	registerOnce.Do(func() { sql.Register("tunr-fake-mssql", &switchDriver{}) })
}

type switchDriver struct{}

func (switchDriver) Open(name string) (driver.Conn, error) { return current.Open(name) }

// ── helpers ──────────────────────────────────────────────────────────────────

const secret = "s3cret-from-tunr-access"

func startSession(t *testing.T, f *fakeDB, lim Limits) (*query.Conn, chan Audit, chan error) {
	t.Helper()
	current = f
	agent, conn := net.Pipe()
	audits := make(chan Audit, 32)
	cfg := Config{ClientSecretHash: hashOf(secret), Limits: lim, Audit: func(a Audit) { audits <- a },
		Open: func(context.Context) (*sql.DB, error) { return sql.Open("tunr-fake-mssql", "") }}
	done := make(chan error, 1)
	go func() { done <- Serve(context.Background(), conn, cfg) }()
	t.Cleanup(func() { _ = agent.Close() })
	return query.NewConn(agent), audits, done
}

func hashOf(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func ask(t *testing.T, qc *query.Conn, sqlText string) query.Response {
	t.Helper()
	if err := qc.Write(query.Request{ID: "1", SQL: sqlText}); err != nil {
		t.Fatal(err)
	}
	var r query.Response
	if err := qc.Read(&r, query.MaxResponse); err != nil {
		t.Fatal(err)
	}
	return r
}

// ── tests ────────────────────────────────────────────────────────────────────

func TestServeAuthAndQuery(t *testing.T) {
	f := &fakeDB{rows: 3}
	qc, audits, _ := startSession(t, f, Limits{})
	if err := qc.Write(query.Auth{Auth: secret}); err != nil {
		t.Fatal(err)
	}
	var h query.Hello
	if err := qc.Read(&h, 4096); err != nil || !h.OK || h.Kind != "mssql" {
		t.Fatalf("hello = %+v, %v", h, err)
	}
	r := ask(t, qc, "SELECT ItemCode, Qty FROM dbo.cdItem")
	if r.Error != nil || r.RowCount != 3 || len(r.ResultSets) != 1 {
		t.Fatalf("response = %+v", r)
	}
	row := r.ResultSets[0].Rows[0]
	if row[0] != "ITEM-A" || row[1].(json.Number).String() != "1" || row[2] != "12.50" ||
		row[3] != "12345678-1234-1234-1234-56789ABCDEF0" {
		t.Errorf("row = %#v", row)
	}
	a := <-audits
	if a.Command != "SELECT" || a.RowCount != 3 || a.Blocked || a.Tables[0] != "dbo.cdItem" {
		t.Errorf("audit = %+v", a)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.rollbacks != 1 || f.commits != 0 {
		t.Errorf("every query must be rolled back: rollbacks=%d commits=%d", f.rollbacks, f.commits)
	}
	if len(f.execs) == 0 || !strings.Contains(f.execs[0], "QUOTED_IDENTIFIER ON") {
		t.Errorf("session setup not run: %v", f.execs)
	}
}

func TestServeRefusesWrongSecret(t *testing.T) {
	qc, _, done := startSession(t, &fakeDB{}, Limits{})
	_ = qc.Write(query.Auth{Auth: "guess"})
	var h query.Hello
	if err := qc.Read(&h, 4096); err != nil || h.OK || h.Error == nil || h.Error.Code != "auth_failed" {
		t.Fatalf("hello = %+v, %v", h, err)
	}
	if err := <-done; err == nil {
		t.Error("session should end")
	}
}

func authed(t *testing.T, f *fakeDB, lim Limits) (*query.Conn, chan Audit, chan error) {
	qc, audits, done := startSession(t, f, lim)
	_ = qc.Write(query.Auth{Auth: secret})
	var h query.Hello
	if err := qc.Read(&h, 4096); err != nil || !h.OK {
		t.Fatalf("hello = %+v, %v", h, err)
	}
	return qc, audits, done
}

func TestServeBlocksWrites(t *testing.T) {
	f := &fakeDB{rows: 1}
	qc, audits, _ := authed(t, f, Limits{})
	r := ask(t, qc, "SELECT 1 DELETE FROM cdItem")
	if r.Error == nil || r.Error.Code != "blocked" {
		t.Fatalf("response = %+v", r)
	}
	if a := <-audits; !a.Blocked || a.Tables[0] != "cdItem" {
		t.Errorf("audit = %+v", a)
	}
	f.mu.Lock()
	n := len(f.queries)
	f.mu.Unlock()
	if n != 0 {
		t.Error("a blocked statement reached the database")
	}
	// The session survives a refusal.
	if r := ask(t, qc, "SELECT 1"); r.Error != nil {
		t.Errorf("after refusal: %+v", r.Error)
	}
}

func TestServeLimits(t *testing.T) {
	f := &fakeDB{rows: 10}
	qc, _, _ := authed(t, f, Limits{MaxRows: 4, QPS: 2})
	r := ask(t, qc, "SELECT * FROM cdItem")
	if r.RowCount != 4 || !r.Truncated || r.Notice == "" {
		t.Errorf("max_rows: %+v", r)
	}
	_ = ask(t, qc, "SELECT 1")
	if r := ask(t, qc, "SELECT 1"); r.Error == nil || r.Error.Code != "rate_limited" {
		t.Errorf("qps: %+v", r)
	}
	time.Sleep(600 * time.Millisecond)
	if r := ask(t, qc, "SELECT 1"); r.Error != nil {
		t.Errorf("qps bucket should refill: %+v", r.Error)
	}
}

func TestServeDataLimitEndsSession(t *testing.T) {
	f := &fakeDB{rows: 50}
	qc, _, done := authed(t, f, Limits{MaxBytes: 100})
	r := ask(t, qc, "SELECT * FROM cdItem")
	if r.Error == nil || r.Error.Code != "data_limit" {
		t.Fatalf("response = %+v", r)
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("session should end after the data limit")
	}
}

func TestServeMultipleResultSetsAndErrors(t *testing.T) {
	f := &fakeDB{rows: 2}
	qc, _, _ := authed(t, f, Limits{})
	r := ask(t, qc, "SELECT 1; SELECT 2")
	if len(r.ResultSets) != 2 || r.RowCount != 4 {
		t.Errorf("result sets: %+v", r)
	}
	r = ask(t, qc, "SELECT boom FROM t")
	if r.Error == nil || r.Error.Code != "db_error" || !strings.Contains(r.Error.Message, "boom") {
		t.Errorf("db error: %+v", r)
	}
}

func TestDSN(t *testing.T) {
	dsn, err := DSN(Config{Upstream: "10.0.0.5:1433", Database: "NEBIM_V3", User: "tunr_agent_ro", Password: "p@ss",
		AppName: "tunr-bridge agt_1", Encrypt: "true", TrustServerCertificate: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"sqlserver://tunr_agent_ro:p%40ss@10.0.0.5:1433", "database=NEBIM_V3", "encrypt=true", "TrustServerCertificate=true"} {
		if !strings.Contains(dsn, want) {
			t.Errorf("dsn %q missing %q", dsn, want)
		}
	}
}

// The agent's QuerySession and local HTTP endpoint against a real connector
// session (fake SQL Server): the two halves of the protocol agree, the
// endpoint needs its token, and a broken stream is reopened once.
func TestAgentSessionAgainstConnector(t *testing.T) {
	f := &fakeDB{rows: 2}
	current = f
	opens := 0
	open := func(context.Context) (*access.Stream, error) {
		opens++
		agent, conn := net.Pipe()
		cfg := Config{ClientSecretHash: hashOf(secret), Audit: func(Audit) {},
			Open: func(context.Context) (*sql.DB, error) { return sql.Open("tunr-fake-mssql", "") }}
		go func() { _ = Serve(context.Background(), conn, cfg) }()
		return access.NewStream(agent), nil
	}
	qs := access.NewQuerySessionWith(open, secret)
	defer qs.Close()
	ctx := context.Background()
	r, err := qs.Query(ctx, "SELECT ItemCode FROM cdItem", 0)
	if err != nil || r.Error != nil || r.RowCount != 2 {
		t.Fatalf("query: %+v %v", r, err)
	}
	if r, _ := qs.Query(ctx, "DROP TABLE cdItem", 0); r == nil || r.Error == nil || r.Error.Code != "blocked" {
		t.Errorf("drop: %+v", r)
	}

	srv := httptest.NewServer(access.QueryHandler(qs, secret))
	defer srv.Close()
	post := func(tok, body string) (int, query.Response) {
		req, _ := http.NewRequest("POST", srv.URL+"/query", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+tok)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out query.Response
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}
	if code, _ := post("wrong", `{"sql":"SELECT 1"}`); code != http.StatusUnauthorized {
		t.Errorf("wrong token = %d", code)
	}
	if code, out := post(secret, `{"sql":"SELECT 1","max_rows":1}`); code != 200 || out.RowCount != 1 || !out.Truncated {
		t.Errorf("http query = %d %+v", code, out)
	}
	if code, _ := post(secret, `{"sql":"EXEC sp_who"}`); code != http.StatusUnprocessableEntity {
		t.Errorf("blocked over http = %d", code)
	}
	if opens != 1 {
		t.Errorf("one stream should carry every query, opened %d", opens)
	}
}
