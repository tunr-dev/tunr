package pgproxy

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// pgEnv: a fresh database with sample tables and a read-only role.
type pgEnv struct {
	upstream, db, role, rolePW string
	audits                     []Audit
	mu                         sync.Mutex
}

func newPGEnv(t *testing.T) *pgEnv {
	t.Helper()
	admin := os.Getenv("TUNR_TEST_DATABASE_URL")
	if admin == "" {
		t.Skip("TUNR_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	suffix := fmt.Sprintf("%d", time.Now().UnixNano()%1e9)
	e := &pgEnv{db: "pgproxy_" + suffix, role: "ro_" + suffix, rolePW: "Rolepw" + suffix + "abcdefghijk"}
	a, err := pgx.Connect(ctx, admin)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close(ctx)
	if _, err := a.Exec(ctx, "CREATE DATABASE "+e.db); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		c, err := pgx.Connect(context.Background(), admin)
		if err == nil {
			_, _ = c.Exec(context.Background(), "DROP DATABASE IF EXISTS "+e.db+" WITH (FORCE)")
			_, _ = c.Exec(context.Background(), "DROP ROLE IF EXISTS "+e.role)
			c.Close(context.Background())
		}
	})
	u, _ := url.Parse(admin)
	e.upstream = u.Host
	u.Path = "/" + e.db
	o, err := pgx.Connect(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	defer o.Close(ctx)
	for _, q := range []string{
		"CREATE TABLE fatura (id int PRIMARY KEY, tutar numeric)",
		"INSERT INTO fatura SELECT g, g * 10 FROM generate_series(1, 42) g",
		"CREATE TABLE big AS SELECT g AS id FROM generate_series(1, 10001) g",
		"CREATE TABLE maas (id int)",
		fmt.Sprintf("CREATE ROLE %s LOGIN PASSWORD '%s'", e.role, e.rolePW),
		fmt.Sprintf("GRANT CONNECT ON DATABASE %s TO %s", e.db, e.role),
		fmt.Sprintf("GRANT USAGE ON SCHEMA public TO %s", e.role),
		fmt.Sprintf("GRANT SELECT ON fatura, big TO %s", e.role),
	} {
		if _, err := o.Exec(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	return e
}

const clientSecret = "local-secret-from-tunr-access"

// serve starts the proxy on one end of a pipe and returns a pgconn config
// whose dialer hands out the other end.
func (e *pgEnv) clientConfig(t *testing.T, password string, lim Limits) *pgconn.Config {
	t.Helper()
	cfg, err := pgconn.ParseConfig("postgres://agent@127.0.0.1:5999/anydb?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Password = password
	cfg.DialFunc = func(ctx context.Context, network, addr string) (net.Conn, error) {
		c1, c2 := net.Pipe()
		go func() {
			_ = Serve(context.Background(), c2, Config{
				Upstream: e.upstream, Database: e.db, User: e.role, Password: e.rolePW, SSLMode: "disable",
				ClientSecretHash: HashSecret(clientSecret), Limits: lim, Logf: t.Logf,
				Audit: func(a Audit) { e.mu.Lock(); e.audits = append(e.audits, a); e.mu.Unlock() },
			})
		}()
		return c1, nil
	}
	return cfg
}

func (e *pgEnv) connect(t *testing.T, lim Limits) *pgx.Conn {
	t.Helper()
	pc := e.clientConfig(t, clientSecret, lim)
	cc, _ := pgx.ParseConfig("postgres://agent@127.0.0.1:5999/anydb?sslmode=disable")
	cc.Config = *pc
	c, err := pgx.ConnectConfig(context.Background(), cc)
	if err != nil {
		t.Fatalf("connect through proxy: %v", err)
	}
	t.Cleanup(func() { c.Close(context.Background()) })
	return c
}

func pgCode(err error) string {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return pe.Code
	}
	return ""
}

func TestProxyAuth(t *testing.T) {
	e := newPGEnv(t)
	_, err := pgconn.ConnectConfig(context.Background(), e.clientConfig(t, "wrong", Limits{}))
	if pgCode(err) != "28P01" {
		t.Fatalf("wrong client secret: %v", err)
	}
}

func TestProxyReadOnly(t *testing.T) {
	e := newPGEnv(t)
	c := e.connect(t, Limits{})
	ctx := context.Background()

	var n int
	if err := c.QueryRow(ctx, "SELECT count(*) FROM fatura").Scan(&n); err != nil || n != 42 {
		t.Fatalf("SELECT (extended): %d %v", n, err)
	}
	if err := c.QueryRow(ctx, "SELECT count(*) FROM fatura WHERE id > $1", 40).Scan(&n); err != nil || n != 2 {
		t.Fatalf("SELECT with params: %d %v", n, err)
	}
	// The session is read-only even for statements the classifier would let
	// through, and the role can't read ungranted tables.
	var ro string
	_ = c.QueryRow(ctx, "SHOW default_transaction_read_only").Scan(&ro)
	if ro != "on" {
		t.Errorf("default_transaction_read_only = %q", ro)
	}
	if _, err := c.Exec(ctx, "SELECT * FROM maas"); pgCode(err) != "42501" {
		t.Errorf("ungranted table: %v", err)
	}

	writes := []string{
		"INSERT INTO fatura VALUES (99, 1)",
		"UPDATE fatura SET tutar = 0",
		"DELETE FROM fatura",
		"SELECT 1; DELETE FROM fatura",
		"WITH x AS (DELETE FROM fatura RETURNING *) SELECT * FROM x",
		"SET default_transaction_read_only = off",
		"COPY fatura TO STDOUT",
	}
	for _, q := range writes {
		// Extended protocol (pgx default)…
		if _, err := c.Exec(ctx, q); pgCode(err) != "42501" {
			t.Errorf("extended %q: %v", q, err)
		}
		// …and simple protocol.
		if _, err := c.Exec(ctx, q, pgx.QueryExecModeSimpleProtocol); pgCode(err) != "42501" {
			t.Errorf("simple %q: %v", q, err)
		}
	}
	// The session survives every refusal.
	if err := c.QueryRow(ctx, "SELECT count(*) FROM fatura").Scan(&n); err != nil || n != 42 {
		t.Fatalf("after refusals: %d %v (data must be untouched)", n, err)
	}
	// Transactions work and stay read-only.
	tx, err := c.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_ = tx.QueryRow(ctx, "SELECT count(*) FROM fatura").Scan(&n)
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestProxyRowLimitAndAudit(t *testing.T) {
	e := newPGEnv(t)
	c := e.connect(t, Limits{MaxRows: 10000})
	ctx := context.Background()

	rows, err := c.Query(ctx, "SELECT id FROM big")
	got := 0
	for err == nil && rows.Next() {
		got++
	}
	if err == nil {
		err = rows.Err()
	}
	if pgCode(err) != "54000" || got > 10000 {
		t.Fatalf("10001-row query with max_rows=10000: got %d rows, err %v", got, err)
	}
	// The connection is resynchronised and usable.
	var n int
	if err := c.QueryRow(ctx, "SELECT count(*) FROM big").Scan(&n); err != nil || n != 10001 {
		t.Fatalf("after row limit: %d %v", n, err)
	}
	if _, err := c.Exec(ctx, "DELETE FROM fatura"); pgCode(err) != "42501" {
		t.Fatal(err)
	}

	time.Sleep(50 * time.Millisecond)
	e.mu.Lock()
	defer e.mu.Unlock()
	var sawCount, sawBlocked bool
	for _, a := range e.audits {
		if a.Command == "SELECT" && a.RowCount == 1 && len(a.Tables) == 1 && a.Tables[0] == "big" {
			sawCount = true
		}
		if a.Blocked && a.Command == "DELETE" && strings.Contains(a.QueryText, "DELETE FROM fatura") {
			sawBlocked = true
		}
	}
	if !sawCount || !sawBlocked {
		t.Errorf("audits = %+v", e.audits)
	}
}

// The default upstream sslmode is "prefer": it must still connect to a
// server without TLS (the test server has none).
func TestProxyDefaultSSLModeFallsBack(t *testing.T) {
	e := newPGEnv(t)
	c1, c2 := net.Pipe()
	go func() {
		_ = Serve(context.Background(), c2, Config{Upstream: e.upstream, Database: e.db, User: e.role,
			Password: e.rolePW, ClientSecretHash: HashSecret(clientSecret)}) // SSLMode left empty
	}()
	cfg, _ := pgconn.ParseConfig("postgres://agent@127.0.0.1:5999/x?sslmode=disable")
	cfg.Password = clientSecret
	cfg.DialFunc = func(context.Context, string, string) (net.Conn, error) { return c1, nil }
	pc, err := pgconn.ConnectConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("connect with default sslmode: %v", err)
	}
	pc.Close(context.Background())
}

func TestProxyQPS(t *testing.T) {
	e := newPGEnv(t)
	c := e.connect(t, Limits{QPS: 2})
	ctx := context.Background()
	limited := 0
	for i := 0; i < 6; i++ {
		if _, err := c.Exec(ctx, "SELECT 1", pgx.QueryExecModeSimpleProtocol); pgCode(err) == "53400" {
			limited++
		}
	}
	if limited == 0 {
		t.Error("QPS=2 never limited 6 back-to-back queries")
	}
}

// TestProxyPsql drives the real psql binary (SSLRequest, simple protocol).
func TestProxyPsql(t *testing.T) {
	psql, err := exec.LookPath("psql")
	if err != nil {
		t.Skip("psql not installed")
	}
	e := newPGEnv(t)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				_ = Serve(context.Background(), c, Config{Upstream: e.upstream, Database: e.db, User: e.role,
					Password: e.rolePW, SSLMode: "disable", ClientSecretHash: HashSecret(clientSecret)})
			}()
		}
	}()
	port := l.Addr().(*net.TCPAddr).Port
	run := func(sql string) (string, error) {
		cmd := exec.Command(psql, "-h", "127.0.0.1", "-p", fmt.Sprint(port), "-U", "agent", "-d", "x", "-tAc", sql)
		cmd.Env = append(os.Environ(), "PGPASSWORD="+clientSecret, "PGSSLMODE=prefer")
		out, err := cmd.CombinedOutput()
		return strings.TrimSpace(string(out)), err
	}
	if out, err := run("select count(*) from fatura"); err != nil || out != "42" {
		t.Fatalf("psql select: %q %v", out, err)
	}
	if out, err := run("update fatura set tutar = 0"); err == nil || !strings.Contains(out, "read-only") {
		t.Fatalf("psql update: %q %v", out, err)
	}
}
