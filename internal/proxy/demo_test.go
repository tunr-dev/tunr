package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// countingHandler records how many requests reached "the app".
func countingHandler(hits *int) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*hits++
		w.WriteHeader(http.StatusTeapot)
	})
}

func mustRules(t *testing.T, specs ...string) []DemoRule {
	t.Helper()
	rules, err := ParseDemoRules(specs)
	if err != nil {
		t.Fatal(err)
	}
	return rules
}

func TestDemo_KeystrokeAutosaveNeverReachesApp(t *testing.T) {
	hits := 0
	h := DemoMiddleware(countingHandler(&hits))

	for _, draft := range []string{"h", "he", "hel", "hell", "hello"} {
		body := `{"title":"` + draft + `"}`
		req := httptest.NewRequest(http.MethodPost, "/api/notes", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		if rec.Code != http.StatusCreated {
			t.Fatalf("draft %q: want 201, got %d", draft, rec.Code)
		}
		var echoed map[string]string
		if err := json.Unmarshal(rec.Body.Bytes(), &echoed); err != nil || echoed["title"] != draft {
			t.Fatalf("draft %q: want the submitted object echoed, got %s", draft, rec.Body.String())
		}
	}
	if hits != 0 {
		t.Fatalf("app saw %d writes in demo mode", hits)
	}
}

func TestDemo_NonJSONBodyGetsGenericPayload(t *testing.T) {
	hits := 0
	h := DemoMiddleware(countingHandler(&hits))
	req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader("user=a&pass=b"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if hits != 0 || !strings.Contains(rec.Body.String(), "demo_success") {
		t.Fatalf("hits=%d body=%s", hits, rec.Body.String())
	}
}

func TestDemo_GraphQL(t *testing.T) {
	cases := []struct {
		name     string
		body     string
		ctype    string
		wantPass bool
	}{
		{"named query", `{"query":"query Items { items { id } }"}`, "application/json", true},
		{"shorthand query", `{"query":"{ items { id } }"}`, "application/json", true},
		{"keyword inside string", `{"query":"query { search(q: \"mutation\") { id } }"}`, "application/json", true},
		{"keyword inside comment", `{"query":"# mutation later\nquery { me { id } }"}`, "application/json", true},
		{"batched reads", `[{"query":"{ a }"},{"query":"query B { b }"}]`, "application/json", true},
		{"raw document", `query { me { id } }`, "application/graphql", true},
		{"mutation", `{"query":"mutation { deleteItem(id: 1) }"}`, "application/json", false},
		{"query plus mutation", `{"query":"query A { a } mutation B { b }"}`, "application/json", false},
		{"batched with a mutation", `[{"query":"{ a }"},{"query":"mutation { b }"}]`, "application/json", false},
		{"subscription", `{"query":"subscription { feed }"}`, "application/json", false},
		{"persisted query hash", `{"extensions":{"persistedQuery":{"sha256Hash":"abc"}}}`, "application/json", false},
		{"unterminated string", `{"query":"query { a(x: \"oops) }"}`, "application/json", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var gotBody string
			h := DemoMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				b, _ := io.ReadAll(r.Body)
				gotBody = string(b)
			}))
			req := httptest.NewRequest(http.MethodPost, "/graphql", strings.NewReader(tc.body))
			req.Header.Set("Content-Type", tc.ctype)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			passed := rec.Header().Get("X-Tunr-Demo-Mode") == ""
			if passed != tc.wantPass {
				t.Fatalf("passed=%v want %v (body %s)", passed, tc.wantPass, rec.Body.String())
			}
			if passed && gotBody != tc.body {
				t.Fatalf("app got body %q, want it intact", gotBody)
			}
			if !passed && rec.Code != http.StatusOK {
				t.Fatalf("blocked GraphQL should be 200, got %d", rec.Code)
			}
		})
	}
}

func TestDemo_AllowAndBlockRules(t *testing.T) {
	hits := 0
	p := &DemoPolicy{
		Allow: mustRules(t, "POST /api/search", "/api/preview/*"),
		Block: mustRules(t, "GET /logout", "POST /api/search/save"),
	}
	h := p.Middleware(countingHandler(&hits))

	cases := []struct {
		method, path string
		wantPass     bool
	}{
		{"POST", "/api/search", true},
		{"PUT", "/api/search", false},       // allow rule is POST-only
		{"POST", "/api/search/save", false}, // exact rule, and block wins
		{"DELETE", "/api/preview/x", true},  // prefix, any method
		{"GET", "/logout", false},           // a GET with side effects
		{"GET", "/logout/now", true},        // exact match only
		{"POST", "/__tunr/feedback", true},  // tunr's own endpoint
		{"PATCH", "/api/items/1", false},    // default
	}
	for _, tc := range cases {
		hits = 0
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, nil))
		if (hits == 1) != tc.wantPass {
			t.Errorf("%s %s: passed=%v want %v", tc.method, tc.path, hits == 1, tc.wantPass)
		}
	}
}

func TestDemo_WebSocketPolicy(t *testing.T) {
	p := &DemoPolicy{Allow: mustRules(t, "WS /rpc"), Block: mustRules(t, "WS /_next/webpack-hmr")}
	cases := []struct {
		path  string
		sub   []string
		block bool
	}{
		{"/socket.io/", nil, true},
		{"/rpc", nil, false},
		{"/", []string{"vite-hmr"}, false},
		{"/_next/webpack-hmr", nil, true}, // block rule beats the HMR default
		{"/sockjs-node/123", nil, false},
	}
	for _, tc := range cases {
		if got := p.BlocksWSWrites(tc.path, tc.sub); got != tc.block {
			t.Errorf("%s %v: block=%v want %v", tc.path, tc.sub, got, tc.block)
		}
	}

	lp := &LocalProxy{}
	if lp.DemoBlocksWSWrites("/socket.io/", nil) {
		t.Fatal("WS writes must pass when demo mode is off")
	}
}

func TestParseDemoRule_Errors(t *testing.T) {
	for _, bad := range []string{"", "api", "FETCH /x", "POST /a/*/b", "POST /a extra"} {
		if _, err := ParseDemoRule(bad); err == nil {
			t.Errorf("%q: expected an error", bad)
		}
	}
	r, err := ParseDemoRule("* /api*")
	if err != nil || r.Method != "" || !r.Prefix || r.Path != "/api" {
		t.Fatalf("got %+v, %v", r, err)
	}
}

// End to end through LocalProxy against a real loopback server: the policy
// set on LocalProxy.Demo is the one the chain enforces.
func TestLocalProxy_DemoPolicyWired(t *testing.T) {
	var seen []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Method+" "+r.URL.Path)
		_, _ = w.Write([]byte("real"))
	}))
	defer upstream.Close()

	p, err := NewLocalProxy(mustPortFromURL(t, upstream.URL), nil)
	if err != nil {
		t.Fatal(err)
	}
	p.DemoMode = true
	p.Demo = &DemoPolicy{Allow: mustRules(t, "POST /api/search"), Block: mustRules(t, "GET /logout")}
	p.BuildMiddlewareChain()

	for _, mp := range [][2]string{{"POST", "/api/notes"}, {"POST", "/api/search"}, {"GET", "/logout"}, {"GET", "/"}} {
		p.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(mp[0], mp[1], strings.NewReader("{}")))
	}
	if got := strings.Join(seen, ","); got != "POST /api/search,GET /" {
		t.Fatalf("upstream saw %q", got)
	}
}
