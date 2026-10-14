package auth

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// grant.go — Ed25519-signed grant JWTs (roadmap §6).
//
// For every bridge stream the relay signs a short-lived token naming the
// agent, site, resource, grant, mode and limits. The connector verifies it
// with the relay public key pinned at install time before opening anything.
//
// Limit of this design, stated plainly: the private key lives on the relay,
// so this protects against relay *bugs* (a stream opened for the wrong grant,
// site or resource), not against a relay *compromise*.

// EnvGrantSigningKey holds the base64 Ed25519 seed (32 bytes) or private key
// (64 bytes).
const EnvGrantSigningKey = "TUNR_GRANT_SIGNING_KEY"

// MaxGrantJWTTTL caps grant JWT lifetime; the relay refreshes for open streams.
const MaxGrantJWTTTL = 15 * time.Minute

// RelayKeyPrefix prefixes the printable relay public key.
const RelayKeyPrefix = "ed25519:"

// GrantLimits are per-grant ceilings enforced by the connector.
type GrantLimits struct {
	MaxRows  int   `json:"max_rows,omitempty"`
	MaxBytes int64 `json:"max_bytes,omitempty"`
	QPS      int   `json:"qps,omitempty"`
}

// GrantClaims are the claims of a grant JWT. sub = agent, aud = site,
// jti = access session.
type GrantClaims struct {
	jwt.RegisteredClaims
	Resource string       `json:"res"`
	GrantID  string       `json:"gid"`
	Mode     string       `json:"mode"`
	Limits   *GrantLimits `json:"lim,omitempty"`
}

// GrantSigner signs grant JWTs.
type GrantSigner struct {
	priv   ed25519.PrivateKey
	issuer string
}

// NewGrantSigner parses key (base64 std or URL, padded or not; 32-byte seed
// or 64-byte private key).
func NewGrantSigner(issuer, key string) (*GrantSigner, error) {
	raw, err := decodeB64(strings.TrimSpace(key))
	if err != nil {
		return nil, fmt.Errorf("%s: not base64: %w", EnvGrantSigningKey, err)
	}
	var priv ed25519.PrivateKey
	switch len(raw) {
	case ed25519.SeedSize:
		priv = ed25519.NewKeyFromSeed(raw)
	case ed25519.PrivateKeySize:
		priv = ed25519.PrivateKey(raw)
		// A 64-byte key is seed||public; reject one whose halves disagree.
		if !priv.Public().(ed25519.PublicKey).Equal(ed25519.NewKeyFromSeed(priv.Seed()).Public()) {
			return nil, fmt.Errorf("%s: inconsistent private key", EnvGrantSigningKey)
		}
	default:
		return nil, fmt.Errorf("%s: want 32 or 64 bytes, got %d", EnvGrantSigningKey, len(raw))
	}
	return &GrantSigner{priv: priv, issuer: issuer}, nil
}

// GenerateGrantSigner makes a fresh key (dev mode). It returns the base64
// seed so the operator can persist it; an ephemeral key breaks every pinned
// connector on restart.
func GenerateGrantSigner(issuer string) (*GrantSigner, string, error) {
	seed := make([]byte, ed25519.SeedSize)
	if _, err := rand.Read(seed); err != nil {
		return nil, "", err
	}
	return &GrantSigner{priv: ed25519.NewKeyFromSeed(seed), issuer: issuer},
		base64.StdEncoding.EncodeToString(seed), nil
}

// PublicKey returns the verification key.
func (s *GrantSigner) PublicKey() ed25519.PublicKey {
	return s.priv.Public().(ed25519.PublicKey)
}

// KeyString is the printable public key connectors pin ("ed25519:<b64url>").
func (s *GrantSigner) KeyString() string {
	return RelayKeyPrefix + base64.RawURLEncoding.EncodeToString(s.PublicKey())
}

// Sign signs c, setting iss/iat/nbf/exp. ttl is capped at MaxGrantJWTTTL.
// sub, a single aud, res, gid, jti and mode (ro|rw) are required.
func (s *GrantSigner) Sign(c GrantClaims, ttl time.Duration) (string, error) {
	if c.Subject == "" || len(c.Audience) != 1 || c.Audience[0] == "" ||
		c.Resource == "" || c.GrantID == "" || c.ID == "" {
		return "", errors.New("grant jwt: sub, aud, res, gid and jti are required")
	}
	if c.Mode != "ro" && c.Mode != "rw" {
		return "", fmt.Errorf("grant jwt: mode %q", c.Mode)
	}
	if ttl <= 0 || ttl > MaxGrantJWTTTL {
		ttl = MaxGrantJWTTTL
	}
	now := time.Now()
	c.Issuer = s.issuer
	c.IssuedAt = jwt.NewNumericDate(now)
	c.NotBefore = jwt.NewNumericDate(now)
	c.ExpiresAt = jwt.NewNumericDate(now.Add(ttl))
	return jwt.NewWithClaims(jwt.SigningMethodEdDSA, c).SignedString(s.priv)
}

// ParseRelayKey parses a KeyString.
func ParseRelayKey(s string) (ed25519.PublicKey, error) {
	if !strings.HasPrefix(s, RelayKeyPrefix) {
		return nil, fmt.Errorf("relay key must start with %q", RelayKeyPrefix)
	}
	raw, err := decodeB64(strings.TrimPrefix(s, RelayKeyPrefix))
	if err != nil || len(raw) != ed25519.PublicKeySize {
		return nil, errors.New("relay key: want 32 base64 bytes")
	}
	return ed25519.PublicKey(raw), nil
}

// VerifyGrant checks a grant JWT against the pinned key and the expected
// site. The connector has its own copy of this logic (CLI module); this one
// serves relay tests and tooling.
func VerifyGrant(pub ed25519.PublicKey, tokenStr, siteID string) (*GrantClaims, error) {
	token, err := jwt.ParseWithClaims(tokenStr, &GrantClaims{},
		func(t *jwt.Token) (interface{}, error) { return pub, nil },
		jwt.WithValidMethods([]string{"EdDSA"}),
		jwt.WithAudience(siteID),
		jwt.WithIssuedAt(),
		jwt.WithExpirationRequired(),
	)
	if err != nil {
		return nil, fmt.Errorf("invalid grant: %w", err)
	}
	c, ok := token.Claims.(*GrantClaims)
	if !ok || !token.Valid {
		return nil, errors.New("invalid grant")
	}
	if c.ExpiresAt.Sub(c.IssuedAt.Time) > MaxGrantJWTTTL+time.Minute {
		return nil, errors.New("invalid grant: lifetime too long")
	}
	if c.Resource == "" || c.GrantID == "" || (c.Mode != "ro" && c.Mode != "rw") {
		return nil, errors.New("invalid grant: missing res/gid/mode")
	}
	return c, nil
}

func decodeB64(s string) ([]byte, error) {
	for _, enc := range []*base64.Encoding{
		base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding,
	} {
		if b, err := enc.DecodeString(s); err == nil {
			return b, nil
		}
	}
	return nil, errors.New("invalid base64")
}
