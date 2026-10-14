package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// agent.go — Agent Bridge machine credentials.
//
//   - Agent token: HS256 JWT with the relay secret, typ "agent", aud
//     "tunr-bridge", sub = agent ID, org = vendor org ID, jti = agent_tokens.id.
//     The signature proves who minted it; the DB (agent_tokens) decides
//     whether it is still usable, so revocation works before expiry.
//   - Site token: 32 random bytes, shown once, stored only as a SHA-256 hash.

// TypAgent is the typ claim of agent tokens.
const TypAgent = "agent"

// AgentAudience is the aud claim of agent tokens.
const AgentAudience = "tunr-bridge"

// MaxAgentTokenTTL caps agent token lifetime (roadmap §4 rule 6).
const MaxAgentTokenTTL = 24 * time.Hour

// AgentClaims are the claims of an agent token.
type AgentClaims struct {
	jwt.RegisteredClaims
	Typ   string `json:"typ"`
	OrgID string `json:"org"`
}

// AgentID returns the agent the token was issued to.
func (c *AgentClaims) AgentID() string { return c.Subject }

// TokenID returns the agent_tokens row ID (jti).
func (c *AgentClaims) TokenID() string { return c.ID }

// IssueAgent signs an agent token. expiresAt must be within MaxAgentTokenTTL.
func (j *JWTAuth) IssueAgent(orgID, agentID, tokenID string, expiresAt time.Time) (string, error) {
	now := time.Now()
	if orgID == "" || agentID == "" || tokenID == "" {
		return "", errors.New("agent token: org, agent and token IDs are required")
	}
	if !expiresAt.After(now) || expiresAt.Sub(now) > MaxAgentTokenTTL+time.Minute {
		return "", fmt.Errorf("agent token: lifetime must be within (0, %s]", MaxAgentTokenTTL)
	}
	claims := AgentClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    "tunr.sh",
			Subject:   agentID,
			Audience:  jwt.ClaimStrings{AgentAudience},
			ID:        tokenID,
			IssuedAt:  jwt.NewNumericDate(now),
			NotBefore: jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(expiresAt),
		},
		Typ:   TypAgent,
		OrgID: orgID,
	}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(j.secret)
}

// VerifyAgent checks an agent token's signature, type, audience and expiry.
// It does NOT check revocation — callers must also consult agent_tokens
// (db.AgentTokenUsable).
func (j *JWTAuth) VerifyAgent(tokenStr string) (*AgentClaims, error) {
	token, err := jwt.ParseWithClaims(tokenStr, &AgentClaims{},
		func(t *jwt.Token) (interface{}, error) { return j.secret, nil },
		jwt.WithValidMethods([]string{"HS256"}),
		jwt.WithAudience(AgentAudience),
		jwt.WithIssuedAt(),
		jwt.WithExpirationRequired(),
	)
	if err != nil {
		return nil, fmt.Errorf("invalid agent token: %w", err)
	}
	c, ok := token.Claims.(*AgentClaims)
	if !ok || !token.Valid {
		return nil, errors.New("invalid agent token")
	}
	if c.Typ != TypAgent {
		return nil, errors.New("invalid agent token: not an agent token")
	}
	if c.Subject == "" || c.OrgID == "" || c.ID == "" {
		return nil, errors.New("invalid agent token: missing sub/org/jti")
	}
	return c, nil
}

// SiteTokenPrefix marks site tokens so they are recognisable in config files
// and secret scanners.
const SiteTokenPrefix = "tunr_site_"

// GenerateSiteToken returns a new site token and the hash to store.
func GenerateSiteToken() (token, hash string, err error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", "", fmt.Errorf("site token: %w", err)
	}
	token = SiteTokenPrefix + base64.RawURLEncoding.EncodeToString(b)
	return token, HashSiteToken(token), nil
}

// HashSiteToken is the stored form of a site token. A plain SHA-256 is enough:
// the token has 256 bits of entropy, so there is nothing to brute-force.
func HashSiteToken(token string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(token)))
	return hex.EncodeToString(sum[:])
}
