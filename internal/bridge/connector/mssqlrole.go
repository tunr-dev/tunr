package connector

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// mssqlrole.go — `tunr connect mssql-role`: the T-SQL a DBA runs to create
// the read-only login the connector uses for an mssql resource (Nebim, Logo,
// Netsis and most Turkish ERPs run on SQL Server). As with pg-role, the login
// is the real read-only guarantee; the connector's statement filter and the
// rolled-back transaction around every query are extra layers.

// MSSQLRoleSpec describes the login to create.
type MSSQLRoleSpec struct {
	Login    string
	Password string
	Database string
	Schema   string   // default schema for unqualified table names (default "dbo")
	Tables   []string // optional "schema.table" or "table"; empty = every table (db_datareader)
	Masks    []MSSQLMask
}

// MSSQLMask is one SQL Server Dynamic Data Masking rule.
type MSSQLMask struct {
	Table    string // "schema.table" or "table"
	Column   string
	Function string // default() | email() | partial(p,"pad",s) | random(a,b)
}

var (
	msIdentRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_$#@-]{0,127}$`)
	msMaskRe  = regexp.MustCompile(`^(default\(\)|email\(\)|partial\(\d{1,3},\s*"[^"'\\\]\[]{0,32}",\s*\d{1,3}\)|random\(\d{1,9},\s*\d{1,9}\))$`)
)

// msQuote brackets a SQL Server identifier. Only plain names are accepted.
func msQuote(s string) (string, error) {
	if !msIdentRe.MatchString(s) {
		return "", fmt.Errorf("invalid identifier %q (letters, digits, _ $ # @ - only)", s)
	}
	return "[" + s + "]", nil
}

// ParseMSSQLMask parses "schema.table.column" or "schema.table.column=partial(0,\"XXX\",4)".
func ParseMSSQLMask(s string) (MSSQLMask, error) {
	target, fn, hasFn := strings.Cut(s, "=")
	if !hasFn {
		fn = "default()"
	}
	parts := strings.Split(strings.TrimSpace(target), ".")
	var m MSSQLMask
	switch len(parts) {
	case 2:
		m = MSSQLMask{Table: parts[0], Column: parts[1]}
	case 3:
		m = MSSQLMask{Table: parts[0] + "." + parts[1], Column: parts[2]}
	default:
		return MSSQLMask{}, fmt.Errorf("mask %q: want [schema.]table.column[=function]", s)
	}
	m.Function = strings.TrimSpace(fn)
	if !msMaskRe.MatchString(m.Function) {
		return MSSQLMask{}, fmt.Errorf(`mask %q: function must be default(), email(), partial(0,"XXXX",4) or random(1,100)`, s)
	}
	return m, nil
}

// GenerateMSSQLPassword returns a password that also satisfies the Windows
// password policy SQL Server applies with CHECK_POLICY = ON (upper, lower and
// digit characters).
func GenerateMSSQLPassword() (string, error) {
	for i := 0; i < 20; i++ {
		pw, err := GeneratePassword()
		if err != nil {
			return "", err
		}
		if strings.ContainsAny(pw, "ABCDEFGHJKLMNPQRSTUVWXYZ") && strings.ContainsAny(pw, "abcdefghijkmnopqrstuvwxyz") &&
			strings.ContainsAny(pw, "23456789") {
			return pw, nil
		}
	}
	return "", errors.New("could not generate a policy-compliant password")
}

func (s MSSQLRoleSpec) tableName(t string) (string, error) {
	sc, tbl, ok := strings.Cut(t, ".")
	if !ok {
		sc, tbl = s.Schema, t
	}
	qs, err := msQuote(sc)
	if err != nil {
		return "", err
	}
	qt, err := msQuote(tbl)
	if err != nil {
		return "", err
	}
	return qs + "." + qt, nil
}

// SQL renders the script for sqlcmd (GO separates batches).
func (s MSSQLRoleSpec) SQL() (string, error) {
	if s.Schema == "" {
		s.Schema = "dbo"
	}
	login, err := msQuote(s.Login)
	if err != nil {
		return "", err
	}
	db, err := msQuote(s.Database)
	if err != nil {
		return "", err
	}
	if !pwRe.MatchString(s.Password) {
		return "", errors.New("password must be 16-128 alphanumeric characters (use GenerateMSSQLPassword)")
	}
	schema, err := msQuote(s.Schema)
	if err != nil {
		return "", err
	}
	tables := make([]string, 0, len(s.Tables))
	for _, t := range s.Tables {
		q, err := s.tableName(t)
		if err != nil {
			return "", err
		}
		tables = append(tables, q)
	}

	var b strings.Builder
	w := func(format string, a ...any) { fmt.Fprintf(&b, format+"\n", a...) }
	w("-- Read-only login for the tunr connector (SQL Server 2016 or later).")
	w("-- Run as sysadmin, or as a login with ALTER ANY LOGIN and db_owner on %s:", db)
	w("--   sqlcmd -S <server> -U sa -i mssql-role.sql")
	w("USE [master];")
	w("CREATE LOGIN %s WITH PASSWORD = N'%s', CHECK_POLICY = ON, CHECK_EXPIRATION = OFF, DEFAULT_DATABASE = %s;", login, s.Password, db)
	w("GO")
	w("USE %s;", db)
	w("CREATE USER %s FOR LOGIN %s WITH DEFAULT_SCHEMA = %s;", login, login, schema)
	if len(tables) == 0 {
		w("ALTER ROLE [db_datareader] ADD MEMBER %s;", login)
	} else {
		for _, t := range tables {
			w("GRANT SELECT ON %s TO %s;", t, login)
		}
	}
	w("-- Belt and braces: deny writes, code execution and schema changes even if")
	w("-- a role grants them later. DENY wins over GRANT in SQL Server.")
	w("DENY INSERT, UPDATE, DELETE, EXECUTE, ALTER ON DATABASE::%s TO %s;", db, login)
	w("DENY UNMASK TO %s;", login)
	w("GO")
	if len(s.Masks) > 0 {
		w("")
		w("-- ── Dynamic Data Masking ─────────────────────────────────────────────")
		w("-- WARNING: masking is a property of the COLUMN, not of this login. Every")
		w("-- user without UNMASK (or db_owner / sysadmin) sees masked values from now")
		w("-- on. Check which login the ERP application itself uses before running")
		w("-- this part. Masking hides values in results; a user who may filter on")
		w("-- the column can still test guesses with WHERE. For values the agent must")
		w("-- never learn, leave the table out of --tables instead.")
		for _, m := range s.Masks {
			t, err := s.tableName(m.Table)
			if err != nil {
				return "", err
			}
			c, err := msQuote(m.Column)
			if err != nil {
				return "", err
			}
			if !msMaskRe.MatchString(m.Function) {
				return "", fmt.Errorf("mask %s.%s: invalid function %q", m.Table, m.Column, m.Function)
			}
			w("ALTER TABLE %s ALTER COLUMN %s ADD MASKED WITH (FUNCTION = '%s');", t, c, m.Function)
		}
		w("GO")
	}
	return b.String(), nil
}
