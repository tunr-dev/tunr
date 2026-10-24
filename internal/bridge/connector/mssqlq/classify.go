// Package mssqlq serves mssql resources in query mode: the connector logs in
// to SQL Server itself with the read-only login from its secret store, runs
// each statement the agent sends, and returns the rows as JSON
// (internal/bridge/query). The agent never sees the credentials.
//
// Three layers keep an agent from writing, from the inside out:
//  1. the login (tunr connect mssql-role): SELECT only, with DENY on writes,
//     EXECUTE and ALTER — the real guarantee;
//  2. every statement runs inside a transaction that is always rolled back;
//  3. this package's T-SQL classifier, an allow-list over a tokenizer, which
//     also produces the per-query audit summary. When in doubt it denies.
package mssqlq

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// Verdict is the classifier's answer for one batch.
type Verdict struct {
	Allowed bool
	Command string   // first keyword, upper case
	Reason  string   // why it was denied
	Tables  []string // best-effort FROM/JOIN targets, for the audit summary
}

type tokKind int

const (
	tokWord tokKind = iota // keyword or identifier (upper-cased)
	tokQuotedIdent
	tokString
	tokNumber
	tokPunct
)

type token struct {
	kind tokKind
	text string // upper-cased for words
	raw  string // as written (words and quoted identifiers)
}

// tokenize splits a T-SQL batch. ok=false on anything it can't parse with
// confidence (unterminated string, comment or bracket): the caller denies.
// The connector pins QUOTED_IDENTIFIER ON, so "…" is an identifier.
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
			depth := 0 // T-SQL block comments nest
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
		case c == '\'' || ((c == 'N' || c == 'n') && i+1 < n && r[i+1] == '\''):
			if c != '\'' {
				i++
			}
			i++
			closed := false
			for i < n {
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
			toks = append(toks, token{kind: tokString})
		case c == '[' || c == '"':
			end := ']'
			if c == '"' {
				end = '"'
			}
			i++
			var b strings.Builder
			closed := false
			for i < n {
				if r[i] == end {
					if i+1 < n && r[i+1] == end {
						b.WriteRune(end)
						i += 2
						continue
					}
					i++
					closed = true
					break
				}
				b.WriteRune(r[i])
				i++
			}
			if !closed {
				return nil, false
			}
			toks = append(toks, token{tokQuotedIdent, b.String(), b.String()})
		case unicode.IsLetter(c) || c == '_' || c == '@' || c == '#':
			j := i
			for j < n && (unicode.IsLetter(r[j]) || unicode.IsDigit(r[j]) || strings.ContainsRune("_@#$", r[j])) {
				j++
			}
			toks = append(toks, token{tokWord, strings.ToUpper(string(r[i:j])), string(r[i:j])})
			i = j
		case unicode.IsDigit(c):
			for i < n && (unicode.IsDigit(r[i]) || unicode.IsLetter(r[i]) || r[i] == '.') {
				i++ // covers 1.5, 1e3 and 0x1F
			}
			toks = append(toks, token{kind: tokNumber})
		default:
			toks = append(toks, token{kind: tokPunct, text: string(c)})
			i++
		}
	}
	return toks, true
}

// Words that must not appear anywhere outside strings, comments and quoted
// identifiers. A batch must start with SELECT or WITH, but T-SQL needs no
// semicolon between statements ("SELECT 1 DELETE FROM t" is two), so every
// statement-starting keyword that could write, run code, change the session
// or reach outside the database is refused wherever it appears. A column
// literally named like one of these must be [bracketed] — a false refusal is
// the safe failure.
var deniedWords = map[string]bool{
	"INSERT": true, "UPDATE": true, "DELETE": true, "MERGE": true, "TRUNCATE": true, "BULK": true,
	"CREATE": true, "DROP": true, "ALTER": true, "GRANT": true, "REVOKE": true, "DENY": true,
	"INTO": true, "EXEC": true, "EXECUTE": true, "SP_EXECUTESQL": true,
	"OPENROWSET": true, "OPENQUERY": true, "OPENDATASOURCE": true, "OPENXML": true,
	"BACKUP": true, "RESTORE": true, "DBCC": true, "KILL": true, "SHUTDOWN": true, "RECONFIGURE": true,
	"CHECKPOINT": true, "USE": true, "WAITFOR": true, "DECLARE": true, "SET": true,
	"BEGIN": true, "COMMIT": true, "ROLLBACK": true, "SAVE": true, "TRAN": true, "TRANSACTION": true,
	"CURSOR": true, "DEALLOCATE": true, "PRINT": true, "RAISERROR": true, "THROW": true, "GOTO": true,
	"RETURN": true, "UPDATETEXT": true, "WRITETEXT": true, "READTEXT": true, "ENABLE": true, "DISABLE": true,
	"SETUSER": true, "REVERT": true, "EXTERNAL": true, "OUTPUT": true,
	"FN_DBLOG": true, "FN_DUMP_DBLOG": true, "FN_GET_AUDIT_FILE": true, "FN_XE_FILE_TARGET_READ_FILE": true,
	"FN_TRACE_GETTABLE": true, "SYS_EXEC": true,
}

// Classify decides whether a batch may run on a read-only bridge session.
func Classify(sql string) Verdict {
	if !utf8.ValidString(sql) || strings.ContainsRune(sql, 0) {
		return Verdict{Command: "UNKNOWN", Reason: "statement is not valid UTF-8"}
	}
	toks, ok := tokenize(sql)
	if !ok {
		return Verdict{Command: "UNKNOWN", Reason: "could not parse the statement safely"}
	}
	first := "EMPTY"
	for _, t := range toks {
		if t.kind == tokWord {
			first = t.text
			break
		}
		if t.kind != tokPunct || (t.text != "(" && t.text != ";") {
			first = "UNKNOWN"
			break
		}
	}
	tbls := tables(toks)
	deny := func(reason string) Verdict {
		return Verdict{Command: first, Reason: reason, Tables: tbls}
	}
	if first == "EMPTY" {
		return deny("empty statement")
	}
	if first != "SELECT" && first != "WITH" {
		return deny(first + " is not allowed on a read-only bridge (SELECT or WITH only)")
	}
	for i, t := range toks {
		if t.kind == tokWord {
			if deniedWords[t.text] {
				return deny(t.text + " is not allowed on a read-only bridge")
			}
			if strings.HasPrefix(t.text, "XP_") || strings.HasPrefix(t.text, "SP_") {
				return deny("system procedures (" + strings.ToLower(t.text) + ") are not allowed on a read-only bridge")
			}
			if t.text == "NEXT" && i+1 < len(toks) && toks[i+1].kind == tokWord && toks[i+1].text == "VALUE" {
				return deny("NEXT VALUE FOR advances a sequence; not allowed on a read-only bridge")
			}
		}
		if t.kind == tokQuotedIdent && i+1 < len(toks) && toks[i+1].kind == tokPunct && toks[i+1].text == "(" {
			up := strings.ToUpper(t.text)
			if deniedWords[up] || strings.HasPrefix(up, "XP_") || strings.HasPrefix(up, "SP_") {
				return deny("[" + t.text + "]() is not allowed on a read-only bridge")
			}
		}
	}
	if chainTooLong(toks) {
		return deny("cross-database and linked-server names (db.schema.table) are not allowed; query this database only")
	}
	return Verdict{Allowed: true, Command: first, Tables: tbls}
}

// chainTooLong reports a name with three or more parts (db.schema.table,
// server.db.schema.table, db..table): the login only belongs to one database,
// and linked servers may map it to someone with more rights.
func chainTooLong(toks []token) bool {
	parts := 0
	prevDot := false
	for _, t := range toks {
		switch {
		case t.kind == tokPunct && t.text == ".":
			if prevDot {
				parts++ // db..table: an empty middle part
			}
			prevDot = true
			continue
		case t.kind == tokWord || t.kind == tokQuotedIdent:
			if prevDot && parts > 0 {
				parts++
			} else {
				parts = 1
			}
		default:
			parts = 0
		}
		prevDot = false
		if parts >= 3 {
			return true
		}
	}
	return false
}

// tables returns identifiers following FROM / JOIN / APPLY (schema.table
// joined, brackets removed) — the read targets the data owner wants to see.
func tables(toks []token) []string {
	var out []string
	seen := map[string]bool{}
	for i := 0; i+1 < len(toks); i++ {
		if toks[i].kind != tokWord {
			continue
		}
		switch toks[i].text {
		case "FROM", "JOIN", "APPLY":
		default:
			continue
		}
		j := i + 1
		var parts []string
		for j < len(toks) {
			t := toks[j]
			switch t.kind {
			case tokWord, tokQuotedIdent:
				parts = append(parts, t.raw)
			default:
				parts = nil
			}
			if parts == nil || j+1 >= len(toks) || toks[j+1].kind != tokPunct || toks[j+1].text != "." {
				break
			}
			j += 2
		}
		if len(parts) > 0 {
			name := strings.Join(parts, ".")
			if !seen[strings.ToLower(name)] {
				seen[strings.ToLower(name)] = true
				out = append(out, name)
			}
		}
	}
	return out
}
