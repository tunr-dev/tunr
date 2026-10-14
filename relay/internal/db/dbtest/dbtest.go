// Package dbtest gives tests a fresh, fully migrated Postgres database.
//
// Set TUNR_TEST_DATABASE_URL to an admin DSN (one that may CREATE DATABASE),
// e.g. postgres://postgres@127.0.0.1:55432/postgres. When it is unset, tests
// that need a database are skipped, so `go test ./...` stays green on machines
// without Postgres. It is deliberately not DATABASE_URL: a test run must never
// be able to touch the relay's real database.
package dbtest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/tunr-dev/tunr/relay/internal/db"
)

// EnvURL is the admin DSN variable.
const EnvURL = "TUNR_TEST_DATABASE_URL"

// New creates a throwaway database, applies every relay/migrations/*.sql in
// order (twice — update.sh re-runs them on every deploy, so they must be
// re-runnable), and drops it when the test ends.
func New(t testing.TB) *db.DB {
	t.Helper()
	admin := os.Getenv(EnvURL)
	if admin == "" {
		t.Skipf("%s not set; skipping database test", EnvURL)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	b := make([]byte, 6)
	_, _ = rand.Read(b)
	name := "tunr_test_" + hex.EncodeToString(b)

	adminConn, err := pgx.Connect(ctx, admin)
	if err != nil {
		t.Fatalf("connect admin: %v", err)
	}
	if _, err := adminConn.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatalf("create database: %v", err)
	}
	_ = adminConn.Close(ctx)

	u, err := url.Parse(admin)
	if err != nil {
		t.Fatalf("parse %s: %v", EnvURL, err)
	}
	u.Path = "/" + name
	dsn := u.String()

	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect test db: %v", err)
	}
	for pass := 0; pass < 2; pass++ {
		for _, f := range migrationFiles(t) {
			sql, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := conn.Exec(ctx, string(sql)); err != nil {
				t.Fatalf("migration %s (pass %d): %v", filepath.Base(f), pass+1, err)
			}
		}
	}
	_ = conn.Close(ctx)

	d, err := db.New(ctx, dsn)
	if err != nil {
		t.Fatalf("db.New: %v", err)
	}
	t.Cleanup(func() {
		d.Close()
		cctx, ccancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer ccancel()
		if c, err := pgx.Connect(cctx, admin); err == nil {
			_, _ = c.Exec(cctx, "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
			_ = c.Close(cctx)
		}
	})
	return d
}

func migrationFiles(t testing.TB) []string {
	_, here, _, _ := runtime.Caller(0)
	dir := filepath.Join(filepath.Dir(here), "..", "..", "..", "migrations")
	files, err := filepath.Glob(filepath.Join(dir, "*.sql"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no migrations found in %s", dir)
	}
	sort.Strings(files)
	return files
}

// User creates a user row and returns its ID.
func User(t testing.TB, d *db.DB, email string) string {
	t.Helper()
	u, _, err := d.GetOrCreateUser(context.Background(), email)
	if err != nil {
		t.Fatalf("create user %s: %v", email, err)
	}
	return u.ID
}
