// Package profile holds built-in schema profiles for databases an agent is
// likely to meet behind the bridge — Turkish ERPs first (Nebim V3, Logo
// Tiger/Go, Netsis). A profile is a map, not a guarantee: table and column
// names come from public sources and vary between versions and installs.
//
// The customer's connector checks a profile against the real database
// (`tunr connect resource verify`) and announces only the tables it found;
// an unverified profile is always shown to the agent as unverified.
package profile

import (
	"embed"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

//go:embed data/*.json
var data embed.FS

// Profile describes one schema.
type Profile struct {
	Name    string   `json:"name"`  // "nebim-v3"
	Title   string   `json:"title"` // "Nebim V3"
	Kind    string   `json:"kind"`  // resource kind it applies to: mssql | postgres
	Summary string   `json:"summary"`
	Detect  []string `json:"detect"` // tables whose presence identifies the product
	Rules   []string `json:"rules"`  // invariants an agent must respect ("CANCELLED = 0")
	Tables  []Table  `json:"tables"`
	Queries []Query  `json:"queries"`
	Sources []string `json:"sources"`
}

// Table is one table or view. Name may hold placeholders: {FIRM} (three
// digits) and {PERIOD} (two digits), as in Logo's LG_{FIRM}_{PERIOD}_INVOICE.
type Table struct {
	Name       string      `json:"name"`
	Title      string      `json:"title"`
	Columns    [][2]string `json:"columns"` // [name, meaning]
	Joins      []string    `json:"joins,omitempty"`
	Notes      string      `json:"notes,omitempty"`
	Confidence string      `json:"confidence"` // confirmed | likely | unverified
}

// Query is an example read-only query.
type Query struct {
	Title      string `json:"title"`
	SQL        string `json:"sql"`
	Confidence string `json:"confidence"`
}

var (
	loadOnce sync.Once
	byName   map[string]*Profile
	loadErr  error
)

func load() {
	byName = map[string]*Profile{}
	entries, err := data.ReadDir("data")
	if err != nil {
		loadErr = err
		return
	}
	for _, e := range entries {
		b, err := data.ReadFile("data/" + e.Name())
		if err != nil {
			loadErr = err
			return
		}
		var p Profile
		dec := json.NewDecoder(strings.NewReader(string(b)))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&p); err != nil {
			loadErr = fmt.Errorf("profile %s: %w", e.Name(), err)
			return
		}
		byName[p.Name] = &p
	}
}

// Get returns a built-in profile.
func Get(name string) (*Profile, bool) {
	loadOnce.Do(load)
	p, ok := byName[name]
	return p, ok
}

// All returns every built-in profile, by name.
func All() []*Profile {
	loadOnce.Do(load)
	out := make([]*Profile, 0, len(byName))
	for _, p := range byName {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// LoadError reports a malformed embedded profile (tests).
func LoadError() error {
	loadOnce.Do(load)
	return loadErr
}

// Pattern turns a table name with placeholders into a case-insensitive
// regexp matching real names, optionally schema-qualified ("dbo.cdItem").
func Pattern(name string) *regexp.Regexp {
	q := regexp.QuoteMeta(name)
	q = strings.ReplaceAll(q, `\{FIRM\}`, `[0-9]{3}`)
	q = strings.ReplaceAll(q, `\{PERIOD\}`, `[0-9]{2}`)
	return regexp.MustCompile(`(?i)^(?:[A-Za-z0-9_]+\.)?` + q + `$`)
}

// Match returns, for each profile table, the real names in present that it
// matches (present is a list of table names from the database).
func (p *Profile) Match(present []string) map[string][]string {
	out := map[string][]string{}
	for _, t := range p.Tables {
		re := Pattern(t.Name)
		for _, name := range present {
			if re.MatchString(name) {
				out[t.Name] = append(out[t.Name], name)
			}
		}
	}
	return out
}

// Detected reports whether present looks like this product.
func (p *Profile) Detected(present []string) bool {
	if len(p.Detect) == 0 {
		return false
	}
	for _, d := range p.Detect {
		re := Pattern(d)
		found := false
		for _, name := range present {
			if re.MatchString(name) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// Description is what an agent gets from tunr_describe_resource.
type Description struct {
	Profile    string      `json:"profile"`
	Title      string      `json:"title"`
	Summary    string      `json:"summary"`
	Verified   bool        `json:"verified"`
	VerifiedAt *time.Time  `json:"verified_at,omitempty"`
	Warning    string      `json:"warning,omitempty"`
	Rules      []string    `json:"rules"`
	Tables     []Described `json:"tables"`
	NotFound   []string    `json:"not_found,omitempty"` // profile tables the customer's database doesn't have
	Queries    []Query     `json:"example_queries"`
	Sources    []string    `json:"sources"`
}

// Described is a profile table with the real names found for it.
type Described struct {
	Table
	FoundAs []string `json:"found_as,omitempty"`
}

// Describe renders p for an agent. verified holds the table names the
// customer's connector found in the real database (nil = never verified).
func (p *Profile) Describe(verified []string, verifiedAt *time.Time) Description {
	d := Description{Profile: p.Name, Title: p.Title, Summary: p.Summary, Rules: p.Rules,
		Queries: p.Queries, Sources: p.Sources, Verified: verified != nil && verifiedAt != nil, VerifiedAt: verifiedAt}
	if !d.Verified {
		d.Warning = "This profile has NOT been checked against this customer's database. Table and column names " +
			"come from public sources and differ between versions: confirm them with INFORMATION_SCHEMA.TABLES / " +
			"INFORMATION_SCHEMA.COLUMNS before relying on a query."
		for _, t := range p.Tables {
			d.Tables = append(d.Tables, Described{Table: t})
		}
		return d
	}
	found := p.Match(verified)
	for _, t := range p.Tables {
		if names := found[t.Name]; len(names) > 0 {
			d.Tables = append(d.Tables, Described{Table: t, FoundAs: names})
		} else {
			d.NotFound = append(d.NotFound, t.Name)
		}
	}
	d.Warning = "Tables were checked against this customer's database; columns were not. Confirm a column with " +
		"INFORMATION_SCHEMA.COLUMNS if a query fails. Example queries use placeholder codes (and firm/period 001/01): " +
		"substitute the names in found_as."
	return d
}
