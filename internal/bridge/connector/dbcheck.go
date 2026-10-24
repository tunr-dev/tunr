package connector

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
	mssql "github.com/microsoft/go-mssqldb"

	"github.com/tunr-dev/tunr/internal/auth"
	"github.com/tunr-dev/tunr/internal/bridge/connector/mssqlq"
)

// dbcheck.go — `tunr connect resource verify`: log in with the resource's
// stored credential, as the connector will, and report (1) whether that login
// could write — the role is the real read-only guarantee, so a login that can
// INSERT is a configuration error — and (2) which tables it can see, which is
// what a schema profile is checked against. Runs on the customer's machine;
// nothing here is sent anywhere unless the customer saves and restarts.

// DBCheck is the result.
type DBCheck struct {
	Login    string   // the login actually used
	Problems []string // ways the login is not read-only
	Tables   []string // "schema.table" visible to the login (tables and views)
}

// ReadOnly reports whether no problem was found.
func (c *DBCheck) ReadOnly() bool { return len(c.Problems) == 0 }

// CheckDatabase connects to res with its stored credential.
func CheckDatabase(ctx context.Context, res *Resource, secrets auth.SecretStore) (*DBCheck, error) {
	if !res.IsDatabase() {
		return nil, fmt.Errorf("%s is a %s resource; only postgres and mssql can be checked", res.Name, res.Kind)
	}
	if res.Credential == "" {
		return nil, fmt.Errorf("no credential stored for %s (tunr connect %s-role --resource %s)", res.Name,
			map[string]string{"postgres": "pg", "mssql": "mssql"}[res.Kind], res.Name)
	}
	raw, err := secrets.Get(res.Credential)
	if err != nil {
		return nil, fmt.Errorf("credential %s: %w", res.Credential, err)
	}
	var cred struct{ User, Password string }
	if err := json.Unmarshal([]byte(raw), &cred); err != nil || cred.User == "" {
		return nil, fmt.Errorf("credential %s is not {user, password}", res.Credential)
	}
	if res.Kind == "mssql" {
		return checkMSSQL(ctx, res, cred.User, cred.Password)
	}
	return checkPostgres(ctx, res, cred.User, cred.Password)
}

func checkMSSQL(ctx context.Context, res *Resource, user, password string) (*DBCheck, error) {
	dsn, err := mssqlq.DSN(mssqlq.Config{Upstream: res.Target, Database: res.Database, User: user, Password: password,
		Encrypt: res.Encrypt, TrustServerCertificate: res.TrustServerCertificate, AppName: "tunr-connect-verify"})
	if err != nil {
		return nil, err
	}
	c, err := mssql.NewConnector(dsn)
	if err != nil {
		return nil, err
	}
	db := sql.OpenDB(c)
	defer db.Close()
	out := &DBCheck{}
	var sysadmin, owner, dbIns, dbUpd, dbDel, dbExec, dbAlter, unmask sql.NullInt64
	err = db.QueryRowContext(ctx, `SELECT SUSER_SNAME(), IS_SRVROLEMEMBER('sysadmin'), IS_ROLEMEMBER('db_owner'),
		HAS_PERMS_BY_NAME(DB_NAME(), 'DATABASE', 'INSERT'), HAS_PERMS_BY_NAME(DB_NAME(), 'DATABASE', 'UPDATE'),
		HAS_PERMS_BY_NAME(DB_NAME(), 'DATABASE', 'DELETE'), HAS_PERMS_BY_NAME(DB_NAME(), 'DATABASE', 'EXECUTE'),
		HAS_PERMS_BY_NAME(DB_NAME(), 'DATABASE', 'ALTER'), HAS_PERMS_BY_NAME(DB_NAME(), 'DATABASE', 'UNMASK')`).
		Scan(&out.Login, &sysadmin, &owner, &dbIns, &dbUpd, &dbDel, &dbExec, &dbAlter, &unmask)
	if err != nil {
		return nil, fmt.Errorf("log in as %s: %w", user, err)
	}
	flag := func(v sql.NullInt64, msg string) {
		if v.Valid && v.Int64 == 1 {
			out.Problems = append(out.Problems, msg)
		}
	}
	flag(sysadmin, "the login is sysadmin")
	flag(owner, "the user is db_owner")
	flag(dbIns, "INSERT is allowed on the database")
	flag(dbUpd, "UPDATE is allowed on the database")
	flag(dbDel, "DELETE is allowed on the database")
	flag(dbExec, "EXECUTE is allowed on the database (stored procedures can write)")
	flag(dbAlter, "ALTER is allowed on the database")
	flag(unmask, "UNMASK is allowed: Dynamic Data Masking does not apply to this login")

	var writable int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sys.objects o
		WHERE o.type IN ('U', 'V') AND (
		  HAS_PERMS_BY_NAME(QUOTENAME(SCHEMA_NAME(o.schema_id)) + '.' + QUOTENAME(o.name), 'OBJECT', 'INSERT') = 1 OR
		  HAS_PERMS_BY_NAME(QUOTENAME(SCHEMA_NAME(o.schema_id)) + '.' + QUOTENAME(o.name), 'OBJECT', 'UPDATE') = 1 OR
		  HAS_PERMS_BY_NAME(QUOTENAME(SCHEMA_NAME(o.schema_id)) + '.' + QUOTENAME(o.name), 'OBJECT', 'DELETE') = 1)`).
		Scan(&writable); err != nil {
		return nil, err
	}
	if writable > 0 {
		out.Problems = append(out.Problems, fmt.Sprintf("the login can write to %d tables or views", writable))
	}
	rows, err := db.QueryContext(ctx, `SELECT TABLE_SCHEMA, TABLE_NAME FROM INFORMATION_SCHEMA.TABLES`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var sc, t string
		if err := rows.Scan(&sc, &t); err != nil {
			return nil, err
		}
		out.Tables = append(out.Tables, sc+"."+t)
	}
	sort.Strings(out.Tables)
	return out, rows.Err()
}

func checkPostgres(ctx context.Context, res *Resource, user, password string) (*DBCheck, error) {
	host, port, err := net.SplitHostPort(res.Target)
	if err != nil {
		return nil, err
	}
	sslmode := res.SSLMode
	if sslmode == "" {
		sslmode = "prefer"
	}
	u := url.URL{Scheme: "postgres", User: url.UserPassword(user, password), Host: net.JoinHostPort(host, port),
		Path: "/" + res.Database, RawQuery: url.Values{"sslmode": {sslmode}, "application_name": {"tunr-connect-verify"}}.Encode()}
	conn, err := pgx.Connect(ctx, u.String())
	if err != nil {
		return nil, fmt.Errorf("log in as %s: %w", user, err)
	}
	defer conn.Close(context.Background())
	out := &DBCheck{}
	var super, bypass, createRole, createDB bool
	var roDefault string
	if err := conn.QueryRow(ctx, `SELECT current_user, r.rolsuper, r.rolbypassrls, r.rolcreaterole, r.rolcreatedb,
		current_setting('default_transaction_read_only')
		FROM pg_roles r WHERE r.rolname = current_user`).Scan(&out.Login, &super, &bypass, &createRole, &createDB, &roDefault); err != nil {
		return nil, err
	}
	if super {
		out.Problems = append(out.Problems, "the role is a superuser")
	}
	if createRole || createDB {
		out.Problems = append(out.Problems, "the role can create roles or databases")
	}
	if roDefault != "on" {
		out.Problems = append(out.Problems, "default_transaction_read_only is off for the role (the connector still forces it per session)")
	}
	var writable int
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM information_schema.table_privileges
		WHERE grantee = current_user AND privilege_type IN ('INSERT', 'UPDATE', 'DELETE', 'TRUNCATE')`).Scan(&writable); err != nil {
		return nil, err
	}
	if writable > 0 {
		out.Problems = append(out.Problems, fmt.Sprintf("the role holds %d write privileges on tables", writable))
	}
	rows, err := conn.Query(ctx, `SELECT table_schema, table_name FROM information_schema.tables
		WHERE table_schema NOT IN ('pg_catalog', 'information_schema') ORDER BY 1, 2`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var sc, t string
		if err := rows.Scan(&sc, &t); err != nil {
			return nil, err
		}
		out.Tables = append(out.Tables, sc+"."+t)
	}
	return out, rows.Err()
}

// ErrTooManyTables is returned when a profile matches more tables than the
// relay accepts (500).
var ErrTooManyTables = errors.New("more than 500 matching tables")

// ProfileTablesFor picks the profile's tables out of present, de-duplicated.
func ProfileTablesFor(match map[string][]string) []string {
	seen := map[string]bool{}
	var out []string
	for _, names := range match {
		for _, n := range names {
			if !seen[strings.ToLower(n)] {
				seen[strings.ToLower(n)] = true
				out = append(out, n)
			}
		}
	}
	sort.Strings(out)
	return out
}
