// Package pgproxy is the connector's read-only PostgreSQL proxy (P5).
//
// Three layers keep an agent from writing, from the inside out:
//  1. the database role the connector logs in with (tunr connect pg-role):
//     SELECT grants only — the real guarantee;
//  2. every session starts with default_transaction_read_only=on;
//  3. this package's statement classifier, which is also what produces the
//     per-query audit summary.
//
// The classifier is an allow-list over a real tokenizer (comments, strings,
// E” escapes, dollar quotes and quoted identifiers are skipped, statements
// are split on top-level semicolons). When in doubt it denies.
package pgproxy

import (
	"strings"
	"unicode"
)

// Verdict is the classifier's answer for one SQL string.
type Verdict struct {
	Allowed bool
	Command string   // first keyword of the first statement, upper case ("SELECT")
	Reason  string   // why it was denied
	Tables  []string // best-effort FROM/JOIN targets, for the audit summary
}

type tokKind int

const (
	tokWord tokKind = iota // keyword or unquoted identifier (upper-cased)
	tokQuotedIdent
	tokString
	tokNumber
	tokPunct
	tokSemicolon
)

type token struct {
	kind tokKind
	text string
}

// tokenize splits sql into tokens. ok=false on anything it can't parse with
// confidence (unterminated string/comment/dollar quote): the caller denies.
func tokenize(sql string) (toks []token, ok bool) {
	r := []rune(sql)
	n := len(r)
	for i := 0; i < n; {
		c := r[i]
		switch {
		case unicode.IsSpace(c):
			i++
		case c == '-' && i+1 < n && r[i+1] == '-':
			for i < n && r[i] != '\n' {
				i++
			}
		case c == '/' && i+1 < n && r[i+1] == '*':
			depth := 0 // PostgreSQL block comments nest
			for i < n {
				if r[i] == '/' && i+1 < n && r[i+1] == '*' {
					depth++
					i += 2
				} else if r[i] == '*' && i+1 < n && r[i+1] == '/' {
					depth--
					i += 2
					if depth == 0 {
						break
					}
				} else {
					i++
				}
			}
			if depth != 0 {
				return nil, false
			}
		case c == '\'' || ((c == 'E' || c == 'e') && i+1 < n && r[i+1] == '\''):
			escapes := c != '\''
			if escapes {
				i++
			}
			i++
			closed := false
			for i < n {
				if escapes && r[i] == '\\' {
					i += 2
					continue
				}
				if r[i] == '\'' {
					if i+1 < n && r[i+1] == '\'' {
						i += 2
						continue
					}
					i++
					closed = true
					break
				}
				i++
			}
			if !closed {
				return nil, false
			}
			toks = append(toks, token{tokString, ""})
		case (c == 'U' || c == 'u') && i+2 < n && r[i+1] == '&' && (r[i+2] == '"' || r[i+2] == '\''):
			// U&"…" / U&'…': unicode-escaped identifier or string.
			q := r[i+2]
			i += 3
			closed := false
			for i < n {
				if r[i] == q {
					if i+1 < n && r[i+1] == q {
						i += 2
						continue
					}
					i++
					closed = true
					break
				}
				i++
			}
			if !closed {
				return nil, false
			}
			if q == '"' {
				toks = append(toks, token{tokQuotedIdent, ""})
			} else {
				toks = append(toks, token{tokString, ""})
			}
		case c == '"':
			i++
			start := i
			closed := false
			var b strings.Builder
			for i < n {
				if r[i] == '"' {
					if i+1 < n && r[i+1] == '"' {
						b.WriteRune('"')
						i += 2
						continue
					}
					closed = true
					break
				}
				b.WriteRune(r[i])
				i++
			}
			_ = start
			if !closed {
				return nil, false
			}
			i++
			toks = append(toks, token{tokQuotedIdent, b.String()})
		case c == '$' && i+1 < n && (r[i+1] == '$' || unicode.IsLetter(r[i+1]) || r[i+1] == '_'):
			// Dollar quote $tag$…$tag$ (a positional parameter like $1 is a number).
			j := i + 1
			for j < n && (unicode.IsLetter(r[j]) || unicode.IsDigit(r[j]) || r[j] == '_') {
				j++
			}
			if j >= n || r[j] != '$' {
				return nil, false
			}
			tag := string(r[i : j+1])
			end := strings.Index(string(r[j+1:]), tag)
			if end < 0 {
				return nil, false
			}
			i = j + 1 + len([]rune(string(r[j+1:])[:end])) + len([]rune(tag))
			toks = append(toks, token{tokString, ""})
		case c == '$' && i+1 < n && unicode.IsDigit(r[i+1]):
			i++
			for i < n && unicode.IsDigit(r[i]) {
				i++
			}
			toks = append(toks, token{tokNumber, ""})
		case c == ';':
			toks = append(toks, token{tokSemicolon, ";"})
			i++
		case unicode.IsLetter(c) || c == '_':
			j := i
			for j < n && (unicode.IsLetter(r[j]) || unicode.IsDigit(r[j]) || r[j] == '_' || r[j] == '$') {
				j++
			}
			toks = append(toks, token{tokWord, strings.ToUpper(string(r[i:j]))})
			i = j
		case unicode.IsDigit(c):
			for i < n && (unicode.IsDigit(r[i]) || r[i] == '.' || r[i] == 'e' || r[i] == 'E') {
				i++
			}
			toks = append(toks, token{tokNumber, ""})
		default:
			toks = append(toks, token{tokPunct, string(c)})
			i++
		}
	}
	return toks, true
}

// Statements that may start a read-only statement.
var allowedFirst = map[string]bool{
	"SELECT": true, "WITH": true, "VALUES": true, "TABLE": true, "SHOW": true, "EXPLAIN": true,
	"BEGIN": true, "START": true, "COMMIT": true, "END": true, "ROLLBACK": true, "ABORT": true,
	"SAVEPOINT": true, "RELEASE": true, "SET": true, "RESET": true,
	"DECLARE": true, "FETCH": true, "MOVE": true, "CLOSE": true,
}

// Words that must not appear anywhere outside strings, comments and quoted
// identifiers. Only words that matter in the middle of an allowed statement
// are listed (data-modifying CTEs, EXPLAIN ANALYZE/EXPLAIN CREATE TABLE AS,
// SELECT INTO, READ WRITE); statements that merely *start* with a dangerous
// word are already refused by allowedFirst. Common column names (role,
// owner, comment, share, lock) are deliberately not here. A column literally
// named like one of these must be double-quoted — a false refusal is the safe
// failure.
var deniedWords = map[string]bool{
	"INSERT": true, "UPDATE": true, "DELETE": true, "MERGE": true, "TRUNCATE": true, "COPY": true,
	"CREATE": true, "DROP": true, "ALTER": true, "GRANT": true, "REVOKE": true, "INTO": true,
	"CALL": true, "DO": true, "EXECUTE": true, "PREPARE": true, "DEALLOCATE": true,
	"ANALYZE": true, "ANALYSE": true, "WRITE": true, "NOTIFY": true, "LISTEN": true,
}

// Functions with side effects, or that run arbitrary SQL strings, or read
// the server's filesystem. The DB role should not be able to call most of
// these anyway; the list is defence in depth and keeps them out of audits.
var deniedFuncs = map[string]bool{
	"PG_TERMINATE_BACKEND": true, "PG_CANCEL_BACKEND": true, "PG_RELOAD_CONF": true, "PG_ROTATE_LOGFILE": true,
	"SET_CONFIG": true, "LO_IMPORT": true, "LO_EXPORT": true, "LO_UNLINK": true, "LO_CREATE": true, "LO_PUT": true,
	"LO_FROM_BYTEA": true, "LO_OPEN": true, "LOWRITE": true, "PG_READ_FILE": true, "PG_READ_BINARY_FILE": true,
	"PG_LS_DIR": true, "PG_STAT_FILE": true, "DBLINK": true, "DBLINK_EXEC": true, "DBLINK_CONNECT": true,
	"PG_SLEEP": true, "PG_SLEEP_FOR": true, "PG_SLEEP_UNTIL": true, "NEXTVAL": true, "SETVAL": true,
	"PG_ADVISORY_LOCK": true, "PG_ADVISORY_XACT_LOCK": true, "PG_TRY_ADVISORY_LOCK": true,
	"PG_NOTIFY": true, "PG_LOGICAL_EMIT_MESSAGE": true, "PG_SWITCH_WAL": true, "PG_CREATE_RESTORE_POINT": true,
	"PG_STAT_RESET": true, "PG_STAT_RESET_SHARED": true, "PG_BACKUP_START": true, "PG_BACKUP_STOP": true,
	"QUERY_TO_XML": true, "QUERY_TO_XML_AND_XMLSCHEMA": true, "QUERY_TO_XMLSCHEMA": true, "CURSOR_TO_XML": true,
	"TABLE_TO_XML": true, "PG_PROMOTE": true, "PG_CREATE_LOGICAL_REPLICATION_SLOT": true,
	"PG_DROP_REPLICATION_SLOT": true, "PG_REPLICATION_ORIGIN_CREATE": true, "TXID_CURRENT": true,
	"PG_CURRENT_XACT_ID": true, "PG_IMPORT_SYSTEM_COLLATIONS": true,
}

// Session parameters an agent may SET or RESET.
var safeSettings = map[string]bool{
	"SEARCH_PATH": true, "STATEMENT_TIMEOUT": true, "APPLICATION_NAME": true, "CLIENT_ENCODING": true,
	"DATESTYLE": true, "TIMEZONE": true, "EXTRA_FLOAT_DIGITS": true, "INTERVALSTYLE": true,
	"LOCK_TIMEOUT": true, "CLIENT_MIN_MESSAGES": true, "BYTEA_OUTPUT": true, "STANDARD_CONFORMING_STRINGS": true,
}

// Classify decides whether sql may run on a read-only bridge session.
func Classify(sql string) Verdict {
	toks, ok := tokenize(sql)
	if !ok {
		return Verdict{Command: "UNKNOWN", Reason: "could not parse the statement safely"}
	}
	// Split into statements.
	var stmts [][]token
	cur := []token{}
	for _, t := range toks {
		if t.kind == tokSemicolon {
			if len(cur) > 0 {
				stmts = append(stmts, cur)
			}
			cur = []token{}
			continue
		}
		cur = append(cur, t)
	}
	if len(cur) > 0 {
		stmts = append(stmts, cur)
	}
	if len(stmts) == 0 {
		return Verdict{Allowed: true, Command: "EMPTY"}
	}
	v := Verdict{Allowed: true, Command: firstWord(stmts[0])}
	for _, st := range stmts {
		if r := denyReason(st); r != "" {
			return Verdict{Command: firstWord(st), Reason: r, Tables: tables(st)}
		}
		v.Tables = append(v.Tables, tables(st)...)
	}
	return v
}

func firstWord(st []token) string {
	for _, t := range st {
		if t.kind == tokWord {
			return t.text
		}
		if t.kind != tokPunct || t.text != "(" {
			break
		}
	}
	return "UNKNOWN"
}

func denyReason(st []token) string {
	first := firstWord(st)
	if !allowedFirst[first] {
		return first + " is not allowed on a read-only bridge"
	}
	for i, t := range st {
		if t.kind != tokWord {
			continue
		}
		if deniedWords[t.text] {
			return t.text + " is not allowed on a read-only bridge"
		}
		// Row locks: FOR UPDATE (caught above), FOR SHARE, FOR NO KEY UPDATE, FOR KEY SHARE.
		if t.text == "FOR" && i+1 < len(st) && st[i+1].kind == tokWord {
			switch st[i+1].text {
			case "SHARE", "NO", "KEY":
				return "row locks (FOR " + st[i+1].text + ") are not allowed on a read-only bridge"
			}
		}
		if i+1 < len(st) && st[i+1].kind == tokPunct && st[i+1].text == "(" && deniedFuncs[t.text] {
			return strings.ToLower(t.text) + "() is not allowed on a read-only bridge"
		}
	}
	switch first {
	case "SET", "RESET":
		return settingReason(st)
	case "BEGIN", "START":
		// "READ WRITE" is caught by WRITE; ISOLATION LEVEL etc. are fine.
	case "DECLARE":
		// DECLARE c CURSOR FOR SELECT …: the body was scanned above.
	}
	return ""
}

// settingReason allows SET/RESET of the safe parameters only.
func settingReason(st []token) string {
	i := 1
	if i < len(st) && st[i].kind == tokWord && (st[i].text == "SESSION" || st[i].text == "LOCAL") {
		i++
	}
	if i >= len(st) {
		return "incomplete SET"
	}
	t := st[i]
	if t.kind == tokWord && t.text == "TIME" && i+1 < len(st) && st[i+1].kind == tokWord && st[i+1].text == "ZONE" {
		return ""
	}
	if t.kind == tokWord && t.text == "ALL" {
		return "RESET ALL is not allowed (it would drop the read-only default)"
	}
	if t.kind != tokWord || !safeSettings[t.text] {
		name := t.text
		if name == "" {
			name = "this parameter"
		}
		return "SET " + strings.ToLower(name) + " is not allowed on a read-only bridge"
	}
	return ""
}

// tables returns identifiers following FROM / JOIN (schema.table joined).
func tables(st []token) []string {
	var out []string
	for i := 0; i+1 < len(st); i++ {
		if st[i].kind != tokWord || (st[i].text != "FROM" && st[i].text != "JOIN") {
			continue
		}
		j := i + 1
		var parts []string
		for j < len(st) {
			t := st[j]
			switch {
			case t.kind == tokWord:
				parts = append(parts, strings.ToLower(t.text))
			case t.kind == tokQuotedIdent:
				parts = append(parts, t.text)
			default:
				parts = nil
			}
			if parts == nil || j+1 >= len(st) || st[j+1].kind != tokPunct || st[j+1].text != "." {
				break
			}
			j += 2
		}
		if len(parts) > 0 && parts[0] != "lateral" && parts[0] != "only" {
			out = append(out, strings.Join(parts, "."))
		}
	}
	return out
}
