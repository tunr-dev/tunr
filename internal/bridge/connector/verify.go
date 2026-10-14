package connector

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// verify.go — connector-side check of the relay's grant JWT (roadmap §4
// rule 3). Deliberately small and strict: compact JWS, alg EdDSA only, the
// pinned key only, audience = this site, short lifetime. No JWT library, so
// there is no algorithm negotiation to get wrong.

const (
	maxGrantLifetime = 16 * time.Minute // relay signs ≤ 15m; 1m slack
	clockSkew        = 30 * time.Second
)

// GrantClaims are the claims the connector relies on.
type GrantClaims struct {
	Issuer   string   `json:"iss"`
	Subject  string   `json:"sub"` // agent ID
	Audience audience `json:"aud"` // site ID
	Resource string   `json:"res"`
	GrantID  string   `json:"gid"`
	Mode     string   `json:"mode"`
	ID       string   `json:"jti"` // stream ID
	Exp      int64    `json:"exp"`
	Iat      int64    `json:"iat"`
	Nbf      int64    `json:"nbf"`
	Limits   *Limits  `json:"lim,omitempty"`
}

// audience accepts the JWT "aud" as a string or an array of strings.
type audience []string

func (a *audience) UnmarshalJSON(b []byte) error {
	var one string
	if err := json.Unmarshal(b, &one); err == nil {
		*a = audience{one}
		return nil
	}
	var many []string
	if err := json.Unmarshal(b, &many); err != nil {
		return errors.New("aud: want string or array")
	}
	*a = many
	return nil
}

// VerifyGrant checks token against the pinned relay key for siteID.
func VerifyGrant(pub ed25519.PublicKey, token, siteID string, now time.Time) (*GrantClaims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, errors.New("grant: not a compact JWS")
	}
	hb, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return nil, errors.New("grant: bad header encoding")
	}
	var hdr struct {
		Alg  string          `json:"alg"`
		Crit json.RawMessage `json:"crit"`
	}
	if err := json.Unmarshal(hb, &hdr); err != nil || hdr.Alg != "EdDSA" || hdr.Crit != nil {
		return nil, errors.New("grant: header must be alg EdDSA with no crit")
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || len(sig) != ed25519.SignatureSize {
		return nil, errors.New("grant: bad signature encoding")
	}
	if !ed25519.Verify(pub, []byte(parts[0]+"."+parts[1]), sig) {
		return nil, errors.New("grant: signature does not verify with the pinned relay key")
	}
	pb, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, errors.New("grant: bad payload encoding")
	}
	var c GrantClaims
	dec := json.NewDecoder(bytes.NewReader(pb))
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("grant: payload: %w", err)
	}

	if len(c.Audience) != 1 || c.Audience[0] != siteID {
		return nil, errors.New("grant: issued for another site")
	}
	exp, iat := time.Unix(c.Exp, 0), time.Unix(c.Iat, 0)
	switch {
	case c.Exp == 0 || c.Iat == 0:
		return nil, errors.New("grant: exp and iat are required")
	case !now.Before(exp.Add(clockSkew)):
		return nil, errors.New("grant: expired")
	case iat.After(now.Add(clockSkew)) || (c.Nbf != 0 && time.Unix(c.Nbf, 0).After(now.Add(clockSkew))):
		return nil, errors.New("grant: not valid yet")
	case exp.Sub(iat) > maxGrantLifetime:
		return nil, errors.New("grant: lifetime too long")
	case c.Subject == "" || c.Resource == "" || c.GrantID == "" || c.ID == "":
		return nil, errors.New("grant: sub, res, gid and jti are required")
	case c.Mode != "ro" && c.Mode != "rw":
		return nil, fmt.Errorf("grant: mode %q", c.Mode)
	}
	return &c, nil
}
