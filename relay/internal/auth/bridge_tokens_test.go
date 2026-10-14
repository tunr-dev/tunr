package auth_test

import (
	"crypto/ed25519"
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/tunr-dev/tunr/relay/internal/auth"
)

const testSecret = "supersecret-32-characters-minimum!!"

func newJWT(t *testing.T) *auth.JWTAuth {
	t.Helper()
	j, err := auth.NewJWTAuth(testSecret, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return j
}

func TestAgentTokenRoundTrip(t *testing.T) {
	j := newJWT(t)
	tok, err := j.IssueAgent("org_v", "agt_1", "atk_1", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	c, err := j.VerifyAgent(tok)
	if err != nil {
		t.Fatal(err)
	}
	if c.AgentID() != "agt_1" || c.OrgID != "org_v" || c.TokenID() != "atk_1" || c.Typ != auth.TypAgent {
		t.Errorf("claims = %+v", c)
	}
}

func TestAgentTokenLifetime(t *testing.T) {
	j := newJWT(t)
	if _, err := j.IssueAgent("o", "a", "t", time.Now().Add(25*time.Hour)); err == nil {
		t.Error("25h agent token issued; want cap at 24h")
	}
	if _, err := j.IssueAgent("o", "a", "t", time.Now().Add(-time.Second)); err == nil {
		t.Error("already-expired agent token issued")
	}
	if _, err := j.IssueAgent("", "a", "t", time.Now().Add(time.Hour)); err == nil {
		t.Error("agent token without org issued")
	}
}

// The core type-confusion guarantee: one secret signs both kinds, so each
// verifier must refuse the other kind.
func TestTokenTypesAreNotInterchangeable(t *testing.T) {
	j := newJWT(t)
	human, _ := j.Issue("user-1", "u@tunr.sh", "free")
	agent, _ := j.IssueAgent("org_v", "agt_1", "atk_1", time.Now().Add(time.Hour))

	if _, err := j.Verify(agent); err == nil {
		t.Error("agent token accepted by human Verify")
	}
	if _, err := j.VerifyAgent(human); err == nil {
		t.Error("human token accepted by VerifyAgent")
	}

	// A token with typ=agent but no audience, or the right audience but
	// another typ, is not an agent token either.
	forge := func(claims jwt.Claims) string {
		s, _ := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(testSecret))
		return s
	}
	exp := jwt.NewNumericDate(time.Now().Add(time.Hour))
	noAud := forge(&auth.AgentClaims{
		RegisteredClaims: jwt.RegisteredClaims{Subject: "agt_1", ID: "atk_1", ExpiresAt: exp},
		Typ:              auth.TypAgent, OrgID: "org_v",
	})
	if _, err := j.VerifyAgent(noAud); err == nil {
		t.Error("agent token without audience accepted")
	}
	wrongTyp := forge(&auth.AgentClaims{
		RegisteredClaims: jwt.RegisteredClaims{Subject: "agt_1", ID: "atk_1", ExpiresAt: exp,
			Audience: jwt.ClaimStrings{auth.AgentAudience}},
		Typ: "site", OrgID: "org_v",
	})
	if _, err := j.VerifyAgent(wrongTyp); err == nil {
		t.Error("typ=site accepted as agent token")
	}
	if _, err := j.Verify(wrongTyp); err == nil {
		t.Error("typ=site accepted as human token")
	}

	other, _ := auth.NewJWTAuth("another-secret-32-characters-long!!", time.Hour)
	if _, err := other.VerifyAgent(agent); err == nil {
		t.Error("agent token accepted under a different secret")
	}
}

func TestSiteToken(t *testing.T) {
	tok, hash, err := auth.GenerateSiteToken()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(tok, auth.SiteTokenPrefix) || len(hash) != 64 {
		t.Fatalf("token %q hash %q", tok, hash)
	}
	if auth.HashSiteToken(tok) != hash || auth.HashSiteToken(" "+tok+"\n") != hash {
		t.Error("hash not stable")
	}
	tok2, hash2, _ := auth.GenerateSiteToken()
	if tok2 == tok || hash2 == hash {
		t.Error("site tokens repeat")
	}
}

func grantClaims() auth.GrantClaims {
	return auth.GrantClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject: "agt_1", Audience: jwt.ClaimStrings{"site_1"}, ID: "acs_1",
		},
		Resource: "res_1", GrantID: "grt_1", Mode: "ro",
		Limits: &auth.GrantLimits{MaxRows: 10000, QPS: 5},
	}
}

func TestGrantSignVerify(t *testing.T) {
	s, seed, err := auth.GenerateGrantSigner("relay.test")
	if err != nil {
		t.Fatal(err)
	}
	tok, err := s.Sign(grantClaims(), 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	pub, err := auth.ParseRelayKey(s.KeyString())
	if err != nil {
		t.Fatal(err)
	}
	c, err := auth.VerifyGrant(pub, tok, "site_1")
	if err != nil {
		t.Fatal(err)
	}
	if c.GrantID != "grt_1" || c.Resource != "res_1" || c.Limits.MaxRows != 10000 || c.Issuer != "relay.test" {
		t.Errorf("claims = %+v", c)
	}

	// Reloading from the persisted seed yields the same key.
	s2, err := auth.NewGrantSigner("relay.test", seed)
	if err != nil || s2.KeyString() != s.KeyString() {
		t.Errorf("reloaded signer key %v, err %v", s2, err)
	}
}

func TestGrantRejections(t *testing.T) {
	s, _, _ := auth.GenerateGrantSigner("relay.test")
	other, _, _ := auth.GenerateGrantSigner("relay.test")
	pub := s.PublicKey()
	tok, _ := s.Sign(grantClaims(), time.Minute)

	if _, err := auth.VerifyGrant(pub, tok, "site_2"); err == nil {
		t.Error("grant for site_1 accepted by site_2")
	}
	if _, err := auth.VerifyGrant(other.PublicKey(), tok, "site_1"); err == nil {
		t.Error("grant accepted under a different relay key")
	}

	// Flip one byte of the payload.
	parts := strings.Split(tok, ".")
	payload, _ := base64.RawURLEncoding.DecodeString(parts[1])
	payload = []byte(strings.Replace(string(payload), `"ro"`, `"rw"`, 1))
	tampered := parts[0] + "." + base64.RawURLEncoding.EncodeToString(payload) + "." + parts[2]
	if _, err := auth.VerifyGrant(pub, tampered, "site_1"); err == nil {
		t.Error("tampered grant (ro→rw) accepted")
	}

	// Algorithm confusion: HS256 keyed with the public key bytes.
	hs, _ := jwt.NewWithClaims(jwt.SigningMethodHS256, grantClaims()).SignedString([]byte(pub))
	if _, err := auth.VerifyGrant(pub, hs, "site_1"); err == nil {
		t.Error("HS256 grant accepted")
	}

}

func TestGrantExpiredAndOverlong(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	sign := func(iat, exp time.Time) string {
		c := grantClaims()
		c.IssuedAt = jwt.NewNumericDate(iat)
		c.ExpiresAt = jwt.NewNumericDate(exp)
		s, _ := jwt.NewWithClaims(jwt.SigningMethodEdDSA, c).SignedString(priv)
		return s
	}
	now := time.Now()
	if _, err := auth.VerifyGrant(pub, sign(now.Add(-10*time.Minute), now.Add(-time.Minute)), "site_1"); err == nil {
		t.Error("expired grant accepted")
	}
	if _, err := auth.VerifyGrant(pub, sign(now, now.Add(time.Hour)), "site_1"); err == nil {
		t.Error("1h grant accepted; max is 15m")
	}
	if _, err := auth.VerifyGrant(pub, sign(now, now.Add(10*time.Minute)), "site_1"); err != nil {
		t.Errorf("valid 10m grant rejected: %v", err)
	}
}

func TestGrantSignerInput(t *testing.T) {
	s, _, _ := auth.GenerateGrantSigner("relay.test")
	c := grantClaims()
	c.Mode = "admin"
	if _, err := s.Sign(c, time.Minute); err == nil {
		t.Error("mode=admin signed")
	}
	c = grantClaims()
	c.Audience = jwt.ClaimStrings{"site_1", "site_2"}
	if _, err := s.Sign(c, time.Minute); err == nil {
		t.Error("multi-audience grant signed")
	}
	// TTL above the cap is clamped, not honoured.
	tok, _ := s.Sign(grantClaims(), 2*time.Hour)
	if _, err := auth.VerifyGrant(s.PublicKey(), tok, "site_1"); err != nil {
		t.Errorf("clamped grant rejected: %v", err)
	}

	for _, bad := range []string{"", "not-base64!!", base64.StdEncoding.EncodeToString(make([]byte, 16))} {
		if _, err := auth.NewGrantSigner("x", bad); err == nil {
			t.Errorf("NewGrantSigner(%q) accepted", bad)
		}
	}
	if _, err := auth.ParseRelayKey("rsa:abc"); err == nil {
		t.Error("ParseRelayKey accepted wrong prefix")
	}
}
