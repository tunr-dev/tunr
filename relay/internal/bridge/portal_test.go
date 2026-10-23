package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/tunr-dev/tunr/relay/internal/auth"
	"github.com/tunr-dev/tunr/relay/internal/db/dbtest"
)

func newPortalEnv(t *testing.T, dev bool) (*httptest.Server, *Server) {
	t.Helper()
	d := dbtest.New(t)
	j, _ := auth.NewJWTAuth("supersecret-32-characters-minimum!!", time.Hour)
	_, seed, _ := auth.GenerateGrantSigner("x")
	s, err := New(Config{Enabled: true, SigningKey: seed, Issuer: "t", PublicURL: "https://bridge.test", DevMode: dev}, Deps{DB: d, JWT: j})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	s.RegisterRoutes(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, s
}

func post(t *testing.T, url, body string) (int, string) {
	t.Helper()
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestPortalStaticAndCSP(t *testing.T) {
	srv, _ := newPortalEnv(t, false)
	for _, p := range []string{"/portal/", "/portal/app.js", "/portal/app.css"} {
		resp, err := http.Get(srv.URL + p)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 200 || !strings.Contains(resp.Header.Get("Content-Security-Policy"), "script-src 'self'") ||
			resp.Header.Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("%s: %d csp=%q", p, resp.StatusCode, resp.Header.Get("Content-Security-Policy"))
		}
	}
}

// The portal must never build HTML from strings: all server data goes
// through textContent. Guard against regressions.
func TestPortalJSHasNoHTMLSinks(t *testing.T) {
	b, err := portalFiles.ReadFile("portal/app.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, sink := range []string{"innerHTML", "outerHTML", "insertAdjacentHTML", "document.write", "eval(", "new Function"} {
		if strings.Contains(string(b), sink) {
			t.Errorf("app.js uses %s", sink)
		}
	}
	html, _ := portalFiles.ReadFile("portal/index.html")
	if strings.Contains(string(html), "<script>") || strings.Contains(string(html), "onload=") {
		t.Error("index.html has inline script (CSP forbids it)")
	}
}

func TestPortalLoginFlow(t *testing.T) {
	srv, _ := newPortalEnv(t, true) // dev mode, no mail provider → link returned
	code, body := post(t, srv.URL+"/api/v1/bridge/portal/login", `{"email":"Owner@Acme.test"}`)
	if code != 200 || !strings.Contains(body, "dev_link") {
		t.Fatalf("login: %d %s", code, body)
	}
	var lr struct {
		DevLink string `json:"dev_link"`
	}
	_ = json.Unmarshal([]byte(body), &lr)
	link := lr.DevLink
	if !strings.HasPrefix(link, "https://bridge.test/portal/#/verify?") {
		t.Fatalf("link = %q", link)
	}
	q, _ := url.ParseQuery(link[strings.Index(link, "?")+1:])
	if q.Get("email") != "owner@acme.test" || len(q.Get("token")) != 64 {
		t.Fatalf("link = %s", link)
	}
	verify := `{"email":"owner@acme.test","token":"` + q.Get("token") + `"}`
	code, body = post(t, srv.URL+"/api/v1/bridge/portal/verify", verify)
	if code != 200 || !strings.Contains(body, `"token":"ey`) {
		t.Fatalf("verify: %d %s", code, body)
	}
	if code, _ = post(t, srv.URL+"/api/v1/bridge/portal/verify", verify); code != 401 {
		t.Errorf("reused link: %d, want 401", code)
	}
	if code, _ = post(t, srv.URL+"/api/v1/bridge/portal/verify", `{"email":"owner@acme.test","token":"`+strings.Repeat("a", 64)+`"}`); code != 401 {
		t.Errorf("forged token: %d", code)
	}
}

func TestPortalLoginNoDevLinkInProductionAndRateLimit(t *testing.T) {
	srv, _ := newPortalEnv(t, false)
	code, body := post(t, srv.URL+"/api/v1/bridge/portal/login", `{"email":"x@acme.test"}`)
	if code != 200 || strings.Contains(body, "dev_link") || strings.Contains(body, "verify?") {
		t.Fatalf("production login leaked the link: %d %s", code, body)
	}
	if code, _ := post(t, srv.URL+"/api/v1/bridge/portal/login", `{"email":"not-an-email"}`); code != 400 {
		t.Errorf("bad email: %d", code)
	}
	limited := false
	for i := 0; i < 6; i++ {
		if code, _ := post(t, srv.URL+"/api/v1/bridge/portal/login", `{"email":"spam@acme.test"}`); code == 429 {
			limited = true
		}
	}
	if !limited {
		t.Error("6 links for one address in a row were not rate limited")
	}
}

func TestOrgSettings(t *testing.T) {
	e := newP4(t)
	e.call(e.owner, "PATCH", "/orgs/acme/settings", map[string]any{"max_grant_ttl": "48h"}, 400, nil)
	e.call(e.owner, "PATCH", "/orgs/acme/settings", map[string]any{"evil": true}, 400, nil)
	e.call(e.member, "PATCH", "/orgs/acme/settings", map[string]any{"log_query_text": true}, 403, nil)
	var st map[string]any
	e.call(e.owner, "PATCH", "/orgs/acme/settings", map[string]any{"log_query_text": true, "max_grant_ttl": "2h"}, 200, &st)
	if st["log_query_text"] != true || st["max_grant_ttl"] != "2h0m0s" {
		t.Fatalf("settings = %v", st)
	}
	// The new cap applies to requests.
	var errBody struct{ Error string }
	e.call(e.agentTok, "POST", "/agent/grants", map[string]any{"resource": "acme/erp-db", "ttl": "3h", "reason": "x"}, 400, &errBody)
	if errBody.Error != "invalid_ttl" {
		t.Errorf("ttl beyond org cap: %s", errBody.Error)
	}
}

func TestLoginLimiterIsPerIP(t *testing.T) {
	l := &loginLimiter{byMail: map[string][]time.Time{}, byIP: map[string][]time.Time{}}
	now := time.Now()
	for i := 0; i < 200; i++ {
		l.allow("203.0.113.9", fmt.Sprintf("u%d@example.test", i), now)
	}
	if !l.allow("198.51.100.7", "owner@example.test", now) {
		t.Fatal("one address's requests blocked another caller's sign-in")
	}
}

func TestPortalMagicLinkSurvivesNewRequest(t *testing.T) {
	d := dbtest.New(t)
	ctx := context.Background()
	exp := time.Now().Add(15 * time.Minute)
	if err := d.SavePortalMagicToken(ctx, "a@example.test", "h1", exp); err != nil {
		t.Fatal(err)
	}
	if err := d.SavePortalMagicToken(ctx, "a@example.test", "h2", exp); err != nil {
		t.Fatal(err)
	}
	if ok, err := d.ConsumePortalMagicToken(ctx, "a@example.test", "h1"); !ok || err != nil {
		t.Fatalf("first link cancelled by a second request: %v %v", ok, err)
	}
}
