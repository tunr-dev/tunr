package bridge_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tunr-dev/tunr/relay/internal/auth"
	"github.com/tunr-dev/tunr/relay/internal/bridge"
	"github.com/tunr-dev/tunr/relay/internal/db"
	"github.com/tunr-dev/tunr/relay/internal/db/dbtest"
)

const secret = "supersecret-32-characters-minimum!!"

type env struct {
	t   *testing.T
	d   *db.DB
	jwt *auth.JWTAuth
	srv *httptest.Server
}

func newEnv(t *testing.T) *env {
	t.Helper()
	d := dbtest.New(t)
	j, _ := auth.NewJWTAuth(secret, time.Hour)
	_, seed, _ := auth.GenerateGrantSigner("x")
	s, err := bridge.New(bridge.Config{
		Enabled: true, SigningKey: seed, PublicURL: "https://bridge.example.test", Issuer: "example.test",
	}, bridge.Deps{DB: d, JWT: j})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	s.RegisterRoutes(mux)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusTeapot) })
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &env{t: t, d: d, jwt: j, srv: srv}
}

// login creates the user row and returns a human bearer token for it.
func (e *env) login(email string) string {
	e.t.Helper()
	uid := dbtest.User(e.t, e.d, email)
	tok, err := e.jwt.Issue(uid, email, "free")
	if err != nil {
		e.t.Fatal(err)
	}
	return tok
}

// do sends a request and decodes the JSON response into out (if non-nil).
func (e *env) do(tok, method, path string, body any, out any) int {
	e.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, e.srv.URL+"/api/v1/bridge"+path, rd)
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			e.t.Fatalf("%s %s: decode %q: %v", method, path, raw, err)
		}
	}
	return resp.StatusCode
}

func (e *env) want(code int, tok, method, path string, body any, out any) {
	e.t.Helper()
	if got := e.do(tok, method, path, body, out); got != code {
		e.t.Fatalf("%s %s = %d, want %d", method, path, got, code)
	}
}

type idObj struct {
	ID   string `json:"id"`
	Slug string `json:"slug"`
	Name string `json:"name"`
}

// TestAPIFlow walks the P1 happy path: customer org links a vendor, creates a
// site; vendor creates an agent and a token; the vendor sees the catalog.
func TestAPIFlow(t *testing.T) {
	e := newEnv(t)
	cust, vend := e.login("it@acme.test"), e.login("dev@vendor.test")

	// Lazy personal org.
	var orgs struct {
		Orgs []struct {
			idObj
			Personal bool
			Role     string
		}
	}
	e.want(200, cust, "GET", "/orgs", nil, &orgs)
	if len(orgs.Orgs) != 1 || !orgs.Orgs[0].Personal || orgs.Orgs[0].Role != "owner" {
		t.Fatalf("personal org: %+v", orgs)
	}

	var acme, vendor idObj
	e.want(201, cust, "POST", "/orgs", map[string]string{"name": "Acme", "slug": "acme", "kind": "customer"}, &acme)
	e.want(201, vend, "POST", "/orgs", map[string]string{"name": "Vendor", "slug": "vendor-co", "kind": "vendor"}, &vendor)
	e.want(409, vend, "POST", "/orgs", map[string]string{"name": "Dup", "slug": "acme"}, nil)
	e.want(400, vend, "POST", "/orgs", map[string]string{"name": "Bad", "slug": "Bad_Slug"}, nil)

	// Only the customer can link; a vendor-only org cannot link a customer.
	e.want(400, vend, "POST", "/orgs/vendor-co/links", map[string]string{"vendor_slug": "acme"}, nil)
	var link struct{ ID string }
	e.want(201, cust, "POST", "/orgs/acme/links", map[string]string{"vendor_slug": "vendor-co"}, &link)

	// Site: token returned once, never on read.
	var created struct {
		Site     idObj
		Token    string
		RelayKey string `json:"relay_key"`
		Command  string `json:"connect_command"`
	}
	e.want(201, cust, "POST", "/orgs/acme/sites", map[string]string{"name": "istanbul-ofis"}, &created)
	if !strings.HasPrefix(created.Token, auth.SiteTokenPrefix) || !strings.HasPrefix(created.RelayKey, auth.RelayKeyPrefix) ||
		!strings.Contains(created.Command, "--relay-key "+created.RelayKey) {
		t.Fatalf("site create response: %+v", created)
	}
	var raw map[string]any
	e.want(200, cust, "GET", "/orgs/acme/sites/"+created.Site.ID, nil, &raw)
	if _, leaked := raw["token"]; leaked {
		t.Fatal("GET site returned the token")
	}
	if s, err := e.d.GetSiteByTokenHash(context.Background(), auth.HashSiteToken(created.Token)); err != nil || s.ID != created.Site.ID {
		t.Fatalf("token hash lookup: %v %v", s, err)
	}

	// Rotation invalidates the old token.
	var rotated struct{ Token string }
	e.want(200, cust, "POST", "/orgs/acme/sites/"+created.Site.ID+"/rotate-token", nil, &rotated)
	if _, err := e.d.GetSiteByTokenHash(context.Background(), auth.HashSiteToken(created.Token)); err == nil {
		t.Fatal("old site token still valid after rotation")
	}

	// Resources are announced by the connector (P2); simulate it.
	var acmeOrg, _ = e.d.GetOrgBySlug(context.Background(), "acme")
	res, err := e.d.UpsertResource(context.Background(), acmeOrg.ID, created.Site.ID, "erp-db", "postgres")
	if err != nil {
		t.Fatal(err)
	}
	e.want(200, cust, "PATCH", "/orgs/acme/resources/"+res.ID,
		map[string]any{"description": "Logo ERP", "limits": map[string]int{"max_rows": 10000}}, nil)
	e.want(400, cust, "PATCH", "/orgs/acme/resources/"+res.ID,
		map[string]any{"limits": map[string]int{"rows": 1}}, nil)
	e.want(400, cust, "PATCH", "/orgs/acme/resources/"+res.ID,
		map[string]any{"auto_approve": map[string]string{"vendor_org_id": vendor.ID, "mode": "rw", "max_ttl": "1h"}}, nil)
	e.want(200, cust, "PATCH", "/orgs/acme/resources/"+res.ID,
		map[string]any{"auto_approve": map[string]string{"vendor_org_id": vendor.ID, "mode": "ro", "max_ttl": "2h"}}, nil)

	// Vendor: agent + token; customer orgs cannot have agents.
	e.want(400, cust, "POST", "/orgs/acme/agents", map[string]string{"name": "nope"}, nil)
	var agent idObj
	e.want(201, vend, "POST", "/orgs/vendor-co/agents", map[string]string{"name": "invoice-reconciler"}, &agent)
	var issued struct {
		Token   string
		TokenID string `json:"token_id"`
	}
	e.want(400, vend, "POST", "/orgs/vendor-co/agents/"+agent.ID+"/tokens", map[string]string{"ttl": "48h"}, nil)
	e.want(201, vend, "POST", "/orgs/vendor-co/agents/"+agent.ID+"/tokens", map[string]string{"ttl": "2h"}, &issued)
	c, err := e.jwt.VerifyAgent(issued.Token)
	if err != nil || c.AgentID() != agent.ID || c.TokenID() != issued.TokenID {
		t.Fatalf("issued agent token: %+v %v", c, err)
	}

	// An agent token is not a login.
	e.want(401, issued.Token, "GET", "/orgs", nil, nil)

	var cat struct{ Resources []struct{ Address string } }
	e.want(200, vend, "GET", "/orgs/vendor-co/catalog", nil, &cat)
	if len(cat.Resources) != 1 || cat.Resources[0].Address != "acme/erp-db" {
		t.Fatalf("catalog: %+v", cat)
	}

	// Revoke the token; disable the agent → no new tokens.
	e.want(204, vend, "DELETE", "/orgs/vendor-co/agents/"+agent.ID+"/tokens/"+issued.TokenID, nil, nil)
	if ok, _ := e.d.AgentTokenUsable(context.Background(), vendor.ID, agent.ID, issued.TokenID); ok {
		t.Fatal("revoked token still usable")
	}
	e.want(200, vend, "PATCH", "/orgs/vendor-co/agents/"+agent.ID, map[string]string{"status": "disabled"}, nil)
	e.want(404, vend, "POST", "/orgs/vendor-co/agents/"+agent.ID+"/tokens", nil, nil)

	// Either side can revoke the link; the catalog empties.
	e.want(200, vend, "DELETE", "/orgs/vendor-co/links/"+link.ID, nil, nil)
	e.want(200, vend, "GET", "/orgs/vendor-co/catalog", nil, &cat)
	if len(cat.Resources) != 0 {
		t.Fatalf("catalog after unlink: %+v", cat)
	}

	// Audit trail landed with org_id.
	var n int
	if err := e.d.QueryRowForTest(context.Background(),
		`SELECT count(*) FROM audit_log WHERE org_id = $1 AND action LIKE 'bridge.%'`, acmeOrg.ID).Scan(&n); err != nil || n < 4 {
		t.Fatalf("acme audit events = %d, err %v", n, err)
	}
}

// TestAPITenantIsolation: an outsider gets 404 on every org-scoped route, and
// a plain member gets 403 on every admin route.
func TestAPITenantIsolation(t *testing.T) {
	e := newEnv(t)
	owner, outsider, plain := e.login("o@acme.test"), e.login("x@evil.test"), e.login("m@acme.test")

	var acme idObj
	e.want(201, owner, "POST", "/orgs", map[string]string{"name": "Acme", "slug": "acme", "kind": "both"}, &acme)
	e.want(201, owner, "POST", "/orgs/acme/members", map[string]string{"email": "m@acme.test", "role": "member"}, nil)
	var site struct{ Site idObj }
	e.want(201, owner, "POST", "/orgs/acme/sites", map[string]string{"name": "hq-site"}, &site)
	var agent idObj
	e.want(201, owner, "POST", "/orgs/acme/agents", map[string]string{"name": "bot"}, &agent)
	var tok struct {
		TokenID string `json:"token_id"`
	}
	e.want(201, owner, "POST", "/orgs/acme/agents/"+agent.ID+"/tokens", nil, &tok)
	res, _ := e.d.UpsertResource(context.Background(), acme.ID, site.Site.ID, "db", "tcp")

	type call struct {
		method, path string
		body         any
		admin        bool
	}
	calls := []call{
		{"GET", "/orgs/acme", nil, false},
		{"GET", "/orgs/" + acme.ID, nil, false},
		{"GET", "/orgs/acme/members", nil, false},
		{"POST", "/orgs/acme/members", map[string]string{"email": "z@evil.test", "role": "admin"}, true},
		{"GET", "/orgs/acme/links", nil, false},
		{"POST", "/orgs/acme/links", map[string]string{"vendor_slug": "acme"}, true},
		{"DELETE", "/orgs/acme/links/lnk_x", nil, true},
		{"GET", "/orgs/acme/sites", nil, false},
		{"POST", "/orgs/acme/sites", map[string]string{"name": "evil"}, true},
		{"GET", "/orgs/acme/sites/" + site.Site.ID, nil, false},
		{"POST", "/orgs/acme/sites/" + site.Site.ID + "/rotate-token", nil, true},
		{"DELETE", "/orgs/acme/sites/" + site.Site.ID, nil, true},
		{"GET", "/orgs/acme/resources", nil, false},
		{"PATCH", "/orgs/acme/resources/" + res.ID, map[string]string{"status": "disabled"}, true},
		{"GET", "/orgs/acme/catalog", nil, false},
		{"GET", "/orgs/acme/agents", nil, false},
		{"POST", "/orgs/acme/agents", map[string]string{"name": "evil"}, true},
		{"PATCH", "/orgs/acme/agents/" + agent.ID, map[string]string{"status": "disabled"}, true},
		{"GET", "/orgs/acme/agents/" + agent.ID + "/tokens", nil, false},
		{"POST", "/orgs/acme/agents/" + agent.ID + "/tokens", nil, true},
		{"DELETE", "/orgs/acme/agents/" + agent.ID + "/tokens/" + tok.TokenID, nil, true},
	}
	for _, c := range calls {
		if got := e.do(outsider, c.method, c.path, c.body, nil); got != http.StatusNotFound {
			t.Errorf("outsider %s %s = %d, want 404", c.method, c.path, got)
		}
		if got := e.do("", c.method, c.path, c.body, nil); got != http.StatusUnauthorized {
			t.Errorf("anonymous %s %s = %d, want 401", c.method, c.path, got)
		}
		if c.admin {
			if got := e.do(plain, c.method, c.path, c.body, nil); got != http.StatusForbidden {
				t.Errorf("member %s %s = %d, want 403", c.method, c.path, got)
			}
		}
	}

	// After all that, the owner's objects are intact.
	e.want(200, owner, "GET", "/orgs/acme/sites/"+site.Site.ID, nil, nil)
	if ok, _ := e.d.AgentTokenUsable(context.Background(), acme.ID, agent.ID, tok.TokenID); !ok {
		t.Fatal("owner's agent token was revoked by a non-admin")
	}
	// Only an owner may add an owner.
	e.want(201, owner, "POST", "/orgs/acme/members", map[string]string{"email": "a@acme.test", "role": "admin"}, nil)
	admin := e.login("a@acme.test")
	e.want(403, admin, "POST", "/orgs/acme/members", map[string]string{"email": "x@evil.test", "role": "owner"}, nil)
}
