package connector

import (
	"context"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func TestServiceUnits(t *testing.T) {
	p := ServiceParams{BinaryPath: "/usr/local/bin/tunr", ConfigPath: "/etc/tunr/connector.json", Home: "/root"}
	unit, err := SystemdUnit(p)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`ExecStart="/usr/local/bin/tunr" connect --config "/etc/tunr/connector.json"`,
		"Restart=always", "NoNewPrivileges=yes", "ProtectSystem=strict",
		"RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX",
	} {
		if !strings.Contains(unit, want) {
			t.Errorf("systemd unit missing %q:\n%s", want, unit)
		}
	}
	if strings.Contains(unit, "User=") {
		t.Error("User= set when no user given")
	}
	p.User = "tunr"
	if unit, _ := SystemdUnit(p); !strings.Contains(unit, "User=tunr") {
		t.Error("User= missing")
	}

	plist, err := LaunchdPlist(ServiceParams{BinaryPath: "/opt/homebrew/bin/tunr", ConfigPath: "/Users/a/Library/Application Support/tunr/connector.json", Home: "/Users/a"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"<string>" + LaunchdLabel + "</string>", "<string>connect</string>",
		"<string>/Users/a/Library/Application Support/tunr/connector.json</string>", "<key>KeepAlive</key>"} {
		if !strings.Contains(plist, want) {
			t.Errorf("plist missing %q", want)
		}
	}

	for _, evil := range []string{"/tmp/x\nExecStartPre=/bin/sh", `/tmp/"x`, "/tmp/</string><string>evil"} {
		if _, err := SystemdUnit(ServiceParams{BinaryPath: evil, ConfigPath: "/c"}); err == nil {
			t.Errorf("SystemdUnit accepted %q", evil)
		}
		if _, err := LaunchdPlist(ServiceParams{BinaryPath: "/b", ConfigPath: evil}); err == nil {
			t.Errorf("LaunchdPlist accepted %q", evil)
		}
	}
}

func TestPGRoleSQLValidation(t *testing.T) {
	pw, _ := GeneratePassword()
	ok := PGRoleSpec{Role: "tunr_ro", Password: pw, Database: "logo", Tables: []string{"fatura", "public.stok"}}
	sql, err := ok.SQL()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`CREATE ROLE "tunr_ro"`, "default_transaction_read_only = on",
		`GRANT SELECT ON "public"."fatura"`, `GRANT SELECT ON "public"."stok"`, `\connect "logo"`} {
		if !strings.Contains(sql, want) {
			t.Errorf("SQL missing %q", want)
		}
	}
	for name, bad := range map[string]PGRoleSpec{
		"injection in role":  {Role: `x"; DROP ROLE postgres; --`, Password: pw, Database: "d"},
		"injection in table": {Role: "r", Password: pw, Database: "d", Tables: []string{`t; DROP TABLE x`}},
		"quote in password":  {Role: "r", Password: "abc'defghijklmnopq", Database: "d"},
		"short password":     {Role: "r", Password: "short", Database: "d"},
	} {
		if _, err := bad.SQL(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// TestPGRoleIsReadOnly applies the generated SQL to a real Postgres and
// checks the role can read the granted table and nothing else, and cannot
// write. Needs TUNR_TEST_DATABASE_URL (admin DSN); skipped otherwise.
func TestPGRoleIsReadOnly(t *testing.T) {
	admin := os.Getenv("TUNR_TEST_DATABASE_URL")
	if admin == "" {
		t.Skip("TUNR_TEST_DATABASE_URL not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	suffix := time.Now().Format("150405") + strings.ReplaceAll(t.Name(), "/", "")[:4]
	dbName, role := "pgrole_"+strings.ToLower(suffix), "ro_"+strings.ToLower(suffix)

	a, err := pgx.Connect(ctx, admin)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close(ctx)
	mustExec := func(c *pgx.Conn, sql string) {
		t.Helper()
		if _, err := c.Exec(ctx, sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	mustExec(a, "CREATE DATABASE "+dbName)
	t.Cleanup(func() {
		c, err := pgx.Connect(context.Background(), admin)
		if err == nil {
			_, _ = c.Exec(context.Background(), "DROP DATABASE IF EXISTS "+dbName+" WITH (FORCE)")
			_, _ = c.Exec(context.Background(), "DROP ROLE IF EXISTS "+role)
			c.Close(context.Background())
		}
	})
	dsn := func(user, pass, db string) string {
		u, _ := url.Parse(admin)
		if user != "" {
			u.User = url.UserPassword(user, pass)
		}
		u.Path = "/" + db
		return u.String()
	}
	owner, err := pgx.Connect(ctx, dsn("", "", dbName))
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close(ctx)
	mustExec(owner, "CREATE TABLE fatura (id int); INSERT INTO fatura VALUES (1),(2); CREATE TABLE maas (id int)")

	pw, _ := GeneratePassword()
	sql, err := PGRoleSpec{Role: role, Password: pw, Database: dbName, Tables: []string{"fatura"}}.SQL()
	if err != nil {
		t.Fatal(err)
	}
	clusterPart, dbPart, ok := strings.Cut(sql, PGConnectMarker)
	if !ok {
		t.Fatal("no \\connect marker")
	}
	_, dbPart, _ = strings.Cut(dbPart, "\n") // drop the rest of the \connect line
	mustExec(a, clusterPart)
	mustExec(owner, dbPart)

	ro, err := pgx.Connect(ctx, dsn(role, pw, dbName))
	if err != nil {
		t.Fatalf("connect as role: %v", err)
	}
	defer ro.Close(ctx)
	var n int
	if err := ro.QueryRow(ctx, "SELECT count(*) FROM fatura").Scan(&n); err != nil || n != 2 {
		t.Fatalf("SELECT granted table: %d, %v", n, err)
	}
	for _, stmt := range []string{
		"INSERT INTO fatura VALUES (3)",
		"UPDATE fatura SET id = 9",
		"DELETE FROM fatura",
		"SELECT count(*) FROM maas", // not granted
		"CREATE TABLE evil (id int)",
	} {
		if _, err := ro.Exec(ctx, stmt); err == nil {
			t.Errorf("read-only role could run: %s", stmt)
		}
	}
}
