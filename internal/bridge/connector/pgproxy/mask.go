package pgproxy

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgproto3"
)

// mask.go — column masking on the response path (the request is never
// rewritten). A result column is masked when its name matches a masked
// column, or when the server reports it comes from a masked table column
// (RowDescription's table OID and attribute number), which catches aliases
// like SELECT tc_kimlik AS x.
//
// Fail-closed rules, because the proxy only sees what the protocol shows:
//   - a statement that mentions a masked column and returns a computed
//     column (an expression: tc_kimlik || '', left(tc_kimlik, 3), count(*)
//     with a WHERE on it) is refused;
//   - the extended query protocol is refused on a session with masks: its
//     results can arrive without a RowDescription in the same cycle, and
//     binary result formats can't be masked as text.
//
// What masking does not do: stop an agent that may read a table from testing
// guesses with WHERE (SELECT ad FROM cari WHERE tc_kimlik = '…' reveals a
// match by returning a row). For values an agent must never learn, leave the
// column out of the database role's grants.

// Mask is one masked column.
type Mask struct {
	Schema   string // optional
	Table    string // optional: empty = this column name in every table
	Column   string
	Strategy string // redact | partial | hash | null (default redact)
	KeepLast int    // partial: characters kept at the end
}

type colRef struct {
	table uint32
	attr  uint16
}

type masker struct {
	byName map[string]*Mask // lower-case column name → rule
	byRef  map[colRef]*Mask
	key    []byte
}

func newMasker(masks []Mask, key []byte) *masker {
	if len(masks) == 0 {
		return nil
	}
	m := &masker{byName: map[string]*Mask{}, byRef: map[colRef]*Mask{}, key: key}
	for i := range masks {
		m.byName[strings.ToLower(masks[i].Column)] = &masks[i]
	}
	return m
}

// resolve maps each mask to the (table OID, attnum) pairs it covers, before
// the upstream connection is handed to the session.
func (m *masker) resolve(ctx context.Context, conn *pgconn.PgConn) error {
	for _, rule := range m.byName {
		rr := conn.ExecParams(ctx, `
			SELECT a.attrelid::oid, a.attnum
			FROM pg_attribute a
			JOIN pg_class c ON c.oid = a.attrelid
			JOIN pg_namespace n ON n.oid = c.relnamespace
			WHERE a.attname = $1 AND a.attnum > 0 AND NOT a.attisdropped
			  AND ($2 = '' OR c.relname = $2) AND ($3 = '' OR n.nspname = $3)`,
			[][]byte{[]byte(rule.Column), []byte(rule.Table), []byte(rule.Schema)}, nil, nil, nil)
		for rr.NextRow() {
			v := rr.Values()
			var oid, att uint64
			if _, err := fmt.Sscan(string(v[0]), &oid); err != nil {
				return err
			}
			if _, err := fmt.Sscan(string(v[1]), &att); err != nil {
				return err
			}
			m.byRef[colRef{uint32(oid), uint16(att)}] = rule
		}
		if _, err := rr.Close(); err != nil {
			return fmt.Errorf("resolve masked column %s: %w", rule.Column, err)
		}
	}
	return nil
}

// mentions reports whether sql refers to a masked column by name (as a word
// or quoted identifier). Case-insensitive: stricter than the server.
func (m *masker) mentions(sql string) bool {
	toks, ok := tokenize(sql)
	if !ok {
		return true
	}
	for _, t := range toks {
		if (t.kind == tokWord || t.kind == tokQuotedIdent) && m.byName[strings.ToLower(t.text)] != nil {
			return true
		}
	}
	return false
}

// plan returns, per result column, the rule to apply (nil = untouched), and
// whether any column is computed (no source table).
func (m *masker) plan(fields []pgproto3.FieldDescription) (rules []*Mask, computed bool) {
	rules = make([]*Mask, len(fields))
	for i, f := range fields {
		if r := m.byRef[colRef{f.TableOID, f.TableAttributeNumber}]; r != nil && f.TableOID != 0 {
			rules[i] = r
		} else if r := m.byName[strings.ToLower(string(f.Name))]; r != nil {
			rules[i] = r
		}
		if f.TableOID == 0 && rules[i] == nil {
			computed = true
		}
	}
	return rules, computed
}

// apply masks one text-format value.
func (m *masker) apply(rule *Mask, v []byte) []byte {
	switch rule.Strategy {
	case "null":
		return nil
	case "hash":
		h := hmac.New(sha256.New, m.key)
		h.Write(v)
		return []byte("h:" + hex.EncodeToString(h.Sum(nil))[:16])
	case "partial":
		s := string(v)
		n := utf8.RuneCountInString(s)
		keep := rule.KeepLast
		if keep >= n {
			keep = n / 2 // never show the whole value
		}
		r := []rune(s)
		return []byte(strings.Repeat("*", n-keep) + string(r[n-keep:]))
	default:
		return []byte("****")
	}
}

// ValidStrategy reports a known masking strategy ("" = redact).
func ValidStrategy(s string) bool {
	switch s {
	case "", "redact", "partial", "hash", "null":
		return true
	}
	return false
}
