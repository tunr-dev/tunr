package connector

import (
	"crypto/rand"
	"errors"
	"fmt"
	"math/big"
	"regexp"
	"strings"
)

// pgrole.go — `tunr connect pg-role` (P2-T4): the SQL a DBA runs to create
// the read-only role the connector logs in with. The role is the real
// read-only guarantee; pgproxy's statement filter (P5) is a second layer.

// PGRoleSpec describes the role to create.
type PGRoleSpec struct {
	Role     string
	Password string
	Database string
	Schemas  []string
	Tables   []string // optional "schema.table" or "table" (→ first schema); empty = all tables in Schemas
}

// PGConnectMarker separates the cluster-level statements from the in-database
// grants. It is a psql meta-command, so the output is meant for psql -f.
const PGConnectMarker = `\connect`

var (
	pgIdentRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_$]{0,62}$`)
	pwRe      = regexp.MustCompile(`^[A-Za-z0-9]{16,128}$`)
)

// quoteIdent double-quotes a Postgres identifier. Only plain identifiers are
// accepted; anything odd is rejected rather than escaped.
func quoteIdent(s string) (string, error) {
	if !pgIdentRe.MatchString(s) {
		return "", fmt.Errorf("invalid identifier %q (letters, digits, _ and $ only)", s)
	}
	return `"` + s + `"`, nil
}

// GeneratePassword returns a 32-char alphanumeric password (no quoting needed).
func GeneratePassword() (string, error) {
	const alphabet = "ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz23456789"
	var b strings.Builder
	for i := 0; i < 32; i++ {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(alphabet))))
		if err != nil {
			return "", err
		}
		b.WriteByte(alphabet[n.Int64()])
	}
	return b.String(), nil
}

// SQL renders the statements. Role statements are cluster-wide and run first,
// outside any transaction (\connect would abort an open one); the grants
// inside the target database run as one transaction.
func (s PGRoleSpec) SQL() (string, error) {
	role, err := quoteIdent(s.Role)
	if err != nil {
		return "", err
	}
	db, err := quoteIdent(s.Database)
	if err != nil {
		return "", err
	}
	if !pwRe.MatchString(s.Password) {
		return "", errors.New("password must be 16-128 alphanumeric characters (use GeneratePassword)")
	}
	if len(s.Schemas) == 0 {
		s.Schemas = []string{"public"}
	}
	schemas := make([]string, 0, len(s.Schemas))
	for _, sc := range s.Schemas {
		q, err := quoteIdent(sc)
		if err != nil {
			return "", err
		}
		schemas = append(schemas, q)
	}

	var b strings.Builder
	w := func(format string, a ...any) { fmt.Fprintf(&b, format+"\n", a...) }
	w("-- Read-only role for the tunr connector.")
	w("-- Run with psql as a superuser or the database owner: psql -f pg-role.sql")
	w("CREATE ROLE %s LOGIN PASSWORD '%s'", role, s.Password)
	w("  NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT NOREPLICATION NOBYPASSRLS CONNECTION LIMIT 10;")
	w("ALTER ROLE %s SET default_transaction_read_only = on;", role)
	w("ALTER ROLE %s SET statement_timeout = '30s';", role)
	w("ALTER ROLE %s SET idle_in_transaction_session_timeout = '60s';", role)
	w("GRANT CONNECT ON DATABASE %s TO %s;", db, role)
	w("%s %s", PGConnectMarker, db)
	w("BEGIN;")
	for _, sc := range schemas {
		w("GRANT USAGE ON SCHEMA %s TO %s;", sc, role)
	}
	if len(s.Tables) == 0 {
		for _, sc := range schemas {
			w("GRANT SELECT ON ALL TABLES IN SCHEMA %s TO %s;", sc, role)
		}
	} else {
		for _, t := range s.Tables {
			sc, tbl, ok := strings.Cut(t, ".")
			if !ok {
				sc, tbl = s.Schemas[0], t
			}
			qs, err := quoteIdent(sc)
			if err != nil {
				return "", err
			}
			qt, err := quoteIdent(tbl)
			if err != nil {
				return "", err
			}
			w("GRANT SELECT ON %s.%s TO %s;", qs, qt, role)
		}
	}
	w("COMMIT;")
	w("-- If PUBLIC still has CREATE on a schema (the PostgreSQL <= 14 default for")
	w("-- public), any role can create objects there. Sessions are read-only by")
	w("-- default and the connector enforces it too, but consider:")
	w("--   REVOKE CREATE ON SCHEMA public FROM PUBLIC;")
	return b.String(), nil
}
