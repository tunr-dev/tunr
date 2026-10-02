package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"
)

// DemoMiddleware blocks state-mutating requests (POST, PUT, PATCH, DELETE) and
// returns fake 2xx responses so the app looks functional without actually changing anything.
// Your client clicks "Place Order" and feels good — but no order is placed.
func DemoMiddleware(next http.Handler) http.Handler {
	return (&DemoPolicy{}).Middleware(next)
}

// maxDemoBodyBytes caps how much of a blocked request we read to echo it back
// or to sniff a GraphQL operation. Anything larger is blocked without echo.
const maxDemoBodyBytes = 1 << 20

// DemoRule matches requests by method and path.
//
//	"POST /graphql"   — exact path, one method
//	"/api/search*"    — path prefix, any method
//	"WS /socket"      — client→server WebSocket messages on that path
//	"GET /logout"     — as a block rule: a GET that has side effects
type DemoRule struct {
	Method string // upper-case; "" matches any method (including WS)
	Path   string
	Prefix bool // Path ended in '*'
}

// ParseDemoRule parses "[METHOD ]PATH[*]".
func ParseDemoRule(spec string) (DemoRule, error) {
	fields := strings.Fields(spec)
	var r DemoRule
	switch len(fields) {
	case 1:
		r.Path = fields[0]
	case 2:
		r.Method = strings.ToUpper(fields[0])
		r.Path = fields[1]
	default:
		return r, fmt.Errorf("invalid demo rule %q: want \"[METHOD ]/path\" (e.g. \"POST /graphql\" or \"/api/search*\")", spec)
	}
	if r.Method == "*" {
		r.Method = ""
	}
	if r.Method != "" && !isDemoRuleMethod(r.Method) {
		return r, fmt.Errorf("invalid demo rule %q: unknown method %q", spec, fields[0])
	}
	if !strings.HasPrefix(r.Path, "/") {
		return r, fmt.Errorf("invalid demo rule %q: path must start with /", spec)
	}
	if strings.HasSuffix(r.Path, "*") {
		r.Prefix = true
		r.Path = strings.TrimSuffix(r.Path, "*")
	}
	if strings.Contains(r.Path, "*") {
		return r, fmt.Errorf("invalid demo rule %q: '*' is only allowed at the end of the path", spec)
	}
	return r, nil
}

// ParseDemoRules parses a list of rule specs, failing on the first bad one.
func ParseDemoRules(specs []string) ([]DemoRule, error) {
	rules := make([]DemoRule, 0, len(specs))
	for _, s := range specs {
		r, err := ParseDemoRule(s)
		if err != nil {
			return nil, err
		}
		rules = append(rules, r)
	}
	return rules, nil
}

func isDemoRuleMethod(m string) bool {
	switch m {
	case "GET", "HEAD", "OPTIONS", "POST", "PUT", "PATCH", "DELETE", "WS":
		return true
	}
	return false
}

func (r DemoRule) matches(method, path string) bool {
	if r.Method != "" && r.Method != method {
		return false
	}
	if r.Prefix {
		return strings.HasPrefix(path, r.Path)
	}
	return path == r.Path
}

// DemoPolicy decides what --demo lets through. Block rules win over allow
// rules, which win over the defaults:
//   - GET/HEAD/OPTIONS pass
//   - GraphQL documents with no mutation pass (reads that happen to use POST)
//   - every other POST/PUT/PATCH/DELETE is answered by tunr and never reaches the app
//   - client→server WebSocket messages are dropped, except dev-server HMR sockets
type DemoPolicy struct {
	Allow []DemoRule
	Block []DemoRule
}

func matchAny(rules []DemoRule, method, path string) bool {
	for _, r := range rules {
		if r.matches(method, path) {
			return true
		}
	}
	return false
}

// BlocksWSWrites reports whether client→server messages on a WebSocket opened
// at path should be dropped. Server→client traffic is never filtered, so live
// updates keep working in demo mode.
func (p *DemoPolicy) BlocksWSWrites(path string, subprotocols []string) bool {
	if matchAny(p.Block, "WS", path) {
		return true
	}
	if matchAny(p.Allow, "WS", path) {
		return false
	}
	return !isHMRSocket(path, subprotocols)
}

// isHMRSocket recognises dev-server hot-reload sockets. Their client→server
// messages are keepalives, not app writes; dropping them makes Next.js dispose
// the page between edits.
func isHMRSocket(path string, subprotocols []string) bool {
	for _, sp := range subprotocols {
		if sp == "vite-hmr" || sp == "vite-ping" {
			return true
		}
	}
	return strings.HasPrefix(path, "/_next/webpack-hmr") || strings.HasPrefix(path, "/sockjs-node")
}

// Middleware wraps next with the demo policy.
func (p *DemoPolicy) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path

		if matchAny(p.Block, r.Method, path) {
			p.fake(w, r, nil)
			return
		}
		if matchAny(p.Allow, r.Method, path) {
			next.ServeHTTP(w, r)
			return
		}

		// Safe methods pass through untouched
		if r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodOptions {
			next.ServeHTTP(w, r)
			return
		}

		// Always let tunr's own endpoints through
		if path == "/__tunr/feedback" || path == "/__tunr/error" {
			next.ServeHTTP(w, r)
			return
		}

		body, complete := readDemoBody(r)
		if r.Method == http.MethodPost && complete && isGraphQLRead(r, body) {
			r.Body = io.NopCloser(bytes.NewReader(body))
			r.ContentLength = int64(len(body))
			next.ServeHTTP(w, r)
			return
		}
		if !complete {
			body = nil
		}
		p.fake(w, r, body)
	})
}

// readDemoBody reads up to maxDemoBodyBytes. complete is false when the body
// was larger (or unreadable); the request is blocked either way.
func readDemoBody(r *http.Request) (body []byte, complete bool) {
	if r.Body == nil {
		return nil, true
	}
	b, err := io.ReadAll(io.LimitReader(r.Body, maxDemoBodyBytes+1))
	if err != nil || len(b) > maxDemoBodyBytes {
		return nil, false
	}
	return b, true
}

// fake answers a blocked request. When the request carried a JSON object we
// echo it back — most create/update endpoints return the saved resource, so
// optimistic UIs render what the user typed instead of a foreign payload.
func (p *DemoPolicy) fake(w http.ResponseWriter, r *http.Request, body []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Tunr-Demo-Mode", "blocked-mutation")

	status := http.StatusOK
	if r.Method == http.MethodPost {
		status = http.StatusCreated
	}

	if isGraphQLPath(r) || looksLikeGraphQL(body) {
		// A GraphQL mutation: clients expect 200 + {"data": ...}.
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"data":       nil,
			"extensions": map[string]interface{}{"tunr": "demo mode: mutation not executed"},
		})
		return
	}

	if r.Method != http.MethodDelete && isJSONContent(r) {
		var obj map[string]interface{}
		if json.Unmarshal(body, &obj) == nil && obj != nil {
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(obj)
			return
		}
	}

	w.WriteHeader(status)
	// Most frontend libs (React Query, SWR) expect JSON back
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"status":  "demo_success",
		"message": "Mutations are disabled in Tunr Demo Mode. Request intercepted and faked.",
		"tunr":    true,
		"method":  r.Method,
		"path":    r.URL.Path,
	})
}

func mediaType(r *http.Request) string {
	mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	return mt
}

func isJSONContent(r *http.Request) bool {
	mt := mediaType(r)
	return mt == "application/json" || strings.HasSuffix(mt, "+json")
}

func isGraphQLPath(r *http.Request) bool {
	return mediaType(r) == "application/graphql" || strings.HasSuffix(r.URL.Path, "/graphql")
}

type graphQLRequest struct {
	Query string `json:"query"`
}

// graphQLDocuments extracts the query text(s) from a GraphQL-over-HTTP body:
// a single {"query": ...}, a batched array of them, or a raw
// application/graphql document. ok is false when the body isn't GraphQL or a
// query is missing (e.g. a persisted-query hash we can't inspect).
func graphQLDocuments(r *http.Request, body []byte) (docs []string, ok bool) {
	if mediaType(r) == "application/graphql" {
		return []string{string(body)}, len(body) > 0
	}
	if !isJSONContent(r) {
		return nil, false
	}
	trimmed := bytes.TrimSpace(body)
	var reqs []graphQLRequest
	if len(trimmed) > 0 && trimmed[0] == '[' {
		if json.Unmarshal(trimmed, &reqs) != nil {
			return nil, false
		}
	} else {
		var one graphQLRequest
		if json.Unmarshal(trimmed, &one) != nil {
			return nil, false
		}
		reqs = []graphQLRequest{one}
	}
	if len(reqs) == 0 {
		return nil, false
	}
	for _, q := range reqs {
		if strings.TrimSpace(q.Query) == "" {
			return nil, false
		}
		docs = append(docs, q.Query)
	}
	return docs, true
}

func looksLikeGraphQL(body []byte) bool {
	var one graphQLRequest
	return json.Unmarshal(body, &one) == nil && strings.TrimSpace(one.Query) != ""
}

// isGraphQLRead is true when every document in the request contains only
// queries (no mutation, no subscription).
func isGraphQLRead(r *http.Request, body []byte) bool {
	docs, ok := graphQLDocuments(r, body)
	if !ok {
		return false
	}
	for _, d := range docs {
		if !graphQLDocIsReadOnly(d) {
			return false
		}
	}
	return true
}

// graphQLDocIsReadOnly scans the top level of a GraphQL document for
// operation keywords. Strings, block strings and comments are skipped so a
// field argument like "mutation" doesn't fool it. Anything it can't parse is
// treated as a write.
func graphQLDocIsReadOnly(doc string) bool {
	depth := 0
	sawOperation := false
	for i := 0; i < len(doc); {
		c := doc[i]
		switch {
		case c == '#':
			for i < len(doc) && doc[i] != '\n' {
				i++
			}
		case strings.HasPrefix(doc[i:], `"""`):
			end := strings.Index(doc[i+3:], `"""`)
			if end < 0 {
				return false
			}
			i += 3 + end + 3
		case c == '"':
			i++
			for i < len(doc) && doc[i] != '"' {
				if doc[i] == '\\' {
					i++
				}
				i++
			}
			if i >= len(doc) {
				return false
			}
			i++
		case c == '{':
			if depth == 0 {
				sawOperation = true // shorthand query: { ... }
			}
			depth++
			i++
		case c == '}':
			depth--
			if depth < 0 {
				return false
			}
			i++
		case depth == 0 && isNameStart(c):
			j := i
			for j < len(doc) && isNameChar(doc[j]) {
				j++
			}
			switch doc[i:j] {
			case "mutation", "subscription":
				return false
			case "query":
				sawOperation = true
			}
			i = j
		default:
			i++
		}
	}
	return depth == 0 && sawOperation
}

func isNameStart(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func isNameChar(c byte) bool {
	return isNameStart(c) || (c >= '0' && c <= '9')
}
