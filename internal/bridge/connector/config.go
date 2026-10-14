// Package connector is `tunr connect`: the process that runs inside a
// customer's network, dials the relay outbound only, and serves the resources
// listed in its local config — and nothing else.
package connector

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"

	"github.com/tunr-dev/tunr/internal/auth"
	"github.com/tunr-dev/tunr/internal/bridge"
	"github.com/tunr-dev/tunr/internal/tunnel"
)

// EnvInsecureRelay allows an http:// or private-address relay. Only for local
// development and the e2e compose setup.
const EnvInsecureRelay = "TUNR_BRIDGE_INSECURE_RELAY"

// SiteTokenSecret is the secret-store name of the site token.
const SiteTokenSecret = "site-token"

// Config is connector.json. It holds no secrets: the site token and database
// credentials live in the secret store and are referenced by name.
type Config struct {
	Relay       string     `json:"relay"`
	Site        string     `json:"site"`
	RelayKey    string     `json:"relay_key"`    // "ed25519:…", pinned at install
	SecretStore string     `json:"secret_store"` // "keychain" (default) | "file"
	Resources   []Resource `json:"resources"`
}

// Resource is a named target the connector may reach. Target is the ONLY
// address the connector will ever dial for this name.
type Resource struct {
	Name       string  `json:"name"`
	Kind       string  `json:"kind"`                 // postgres | tcp | http
	Target     string  `json:"target"`               // host:port, never sent to the relay
	Database   string  `json:"database,omitempty"`   // postgres
	Credential string  `json:"credential,omitempty"` // secret-store name holding {"user","password"}
	SSLMode    string  `json:"sslmode,omitempty"`    // postgres upstream sslmode (default "prefer")
	Limits     *Limits `json:"limits,omitempty"`
}

// Limits are local ceilings; a grant may narrow them, never widen them.
type Limits struct {
	MaxRows  int   `json:"max_rows,omitempty"`
	MaxBytes int64 `json:"max_bytes,omitempty"`
	QPS      int   `json:"qps,omitempty"`
}

var nameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,30}[a-z0-9]$`)

// ValidName reports whether s is a valid site or resource name.
func ValidName(s string) bool { return nameRe.MatchString(s) && !strings.Contains(s, "--") }

// DefaultPath is where connector.json lives: /etc/tunr when running as root
// on Linux (the systemd case), the user config dir otherwise.
func DefaultPath() string {
	if runtime.GOOS == "linux" && os.Geteuid() == 0 {
		return "/etc/tunr/connector.json"
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return "connector.json"
	}
	return filepath.Join(dir, "tunr", "connector.json")
}

// Load reads and validates a config.
func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("no connector config at %s — run `tunr connect --site … --token … --relay-key …` first", path)
		}
		return nil, err
	}
	var c Config
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if err := c.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &c, nil
}

// Save writes the config atomically with mode 0600.
func (c *Config) Save(path string) error {
	if err := c.Validate(); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(c, "", "  ")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Validate checks every field. It runs on load and save, so a hand-edited
// config can't smuggle in a bad target or an unpinned relay.
func (c *Config) Validate() error {
	if err := ValidateRelay(c.Relay); err != nil {
		return err
	}
	if !ValidName(c.Site) {
		return fmt.Errorf("invalid site name %q", c.Site)
	}
	if _, err := ParseRelayKey(c.RelayKey); err != nil {
		return err
	}
	switch c.SecretStore {
	case "", "keychain", "file":
	default:
		return fmt.Errorf("secret_store must be \"keychain\" or \"file\"")
	}
	seen := map[string]bool{}
	for _, r := range c.Resources {
		if !ValidName(r.Name) {
			return fmt.Errorf("invalid resource name %q", r.Name)
		}
		if seen[r.Name] {
			return fmt.Errorf("duplicate resource %q", r.Name)
		}
		seen[r.Name] = true
		if r.Kind != "postgres" && r.Kind != "tcp" && r.Kind != "http" {
			return fmt.Errorf("resource %s: kind must be postgres, tcp or http", r.Name)
		}
		if err := validTarget(r.Target); err != nil {
			return fmt.Errorf("resource %s: %w", r.Name, err)
		}
		if r.Kind == "postgres" && r.Database == "" {
			return fmt.Errorf("resource %s: postgres needs a database", r.Name)
		}
		switch r.SSLMode {
		case "", "disable", "allow", "prefer", "require", "verify-ca", "verify-full":
		default:
			return fmt.Errorf("resource %s: invalid sslmode %q", r.Name, r.SSLMode)
		}
	}
	if len(c.Resources) > 100 {
		return errors.New("at most 100 resources per site")
	}
	return nil
}

func validTarget(t string) error {
	host, port, err := net.SplitHostPort(t)
	if err != nil || host == "" {
		return fmt.Errorf("target must be host:port, got %q", t)
	}
	if p, err := strconv.Atoi(port); err != nil || p < 1 || p > 65535 {
		return fmt.Errorf("target port %q", port)
	}
	return nil
}

// ValidateRelay requires https (or wss). http and private addresses are
// allowed only with TUNR_BRIDGE_INSECURE_RELAY=1.
func ValidateRelay(raw string) error {
	if os.Getenv(EnvInsecureRelay) == "1" {
		u, err := url.Parse(raw)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https" && u.Scheme != "ws" && u.Scheme != "wss") {
			return fmt.Errorf("invalid relay URL %q", raw)
		}
		return nil
	}
	return tunnel.ValidateRelayURL(raw)
}

// ParseRelayKey parses "ed25519:<base64url>".
func ParseRelayKey(s string) (ed25519.PublicKey, error) {
	rest, ok := strings.CutPrefix(s, "ed25519:")
	if !ok {
		return nil, errors.New(`relay_key must look like "ed25519:…" (copy it from the portal)`)
	}
	for _, enc := range []*base64.Encoding{base64.RawURLEncoding, base64.URLEncoding, base64.RawStdEncoding, base64.StdEncoding} {
		if b, err := enc.DecodeString(rest); err == nil && len(b) == ed25519.PublicKeySize {
			return ed25519.PublicKey(b), nil
		}
	}
	return nil, errors.New("relay_key: not a 32-byte ed25519 public key")
}

// Decls is what the relay is told about resources: names and kinds, nothing else.
func (c *Config) Decls() []bridge.ResourceDecl {
	out := make([]bridge.ResourceDecl, 0, len(c.Resources))
	for _, r := range c.Resources {
		out = append(out, bridge.ResourceDecl{Name: r.Name, Kind: r.Kind})
	}
	return out
}

// Resource returns the named resource.
func (c *Config) Resource(name string) (*Resource, bool) {
	for i := range c.Resources {
		if c.Resources[i].Name == name {
			return &c.Resources[i], true
		}
	}
	return nil, false
}

// Secrets returns the configured secret store. The file store lives next to
// the config, in secrets/.
func (c *Config) Secrets(configPath string) auth.SecretStore {
	if c.SecretStore == "file" {
		return auth.FileStore{Dir: filepath.Join(filepath.Dir(configPath), "secrets")}
	}
	return auth.KeychainStore{}
}

// KillPath is the local kill-switch file for this config.
func KillPath(configPath string) string {
	return filepath.Join(filepath.Dir(configPath), "connector.killed")
}
