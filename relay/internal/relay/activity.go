package relay

// activity.go — request activity classification (Yoğunluk planı Faz 1, lever L7).
//
// The sweeper decides when to sleep an app from "time since last request". That
// is too blunt, and it fails in both directions:
//
//   - Health checks, uptime robots and crawlers hit an app every 30–60 seconds
//     forever. Under a last-request rule they pin every app awake permanently,
//     which quietly cancels scale-to-zero for the entire population.
//   - WebSocket and SSE connections look like a single old request. Under the
//     same rule the app gets frozen mid-stream while a client is still attached.
//
// So a request carries an activity *class*, not just a timestamp:
//
//	Probe   — wakes nothing, doesn't count as activity. A sleeping app answers
//	          these itself, at the edge, with a synthetic 200 (Koyeb's dummy
//	          server trick, but free because the relay is already in the path).
//	Normal  — real traffic. Wakes the app and keeps it warm.
//	Pin     — long-lived connection. Sleep is forbidden while one is open.
//	Crawl   — a search-engine crawler on a real page. It must get the real
//	          page (a synthetic "ok" would be indexed as the page's content),
//	          so it wakes the app for the duration of the request, but it never
//	          resets the idle clock: a crawler alone can't keep an app up.
//
// Deliberately conservative: anything unrecognised is Normal. Misclassifying
// real traffic as a probe would let us freeze an app someone is using, which is
// far worse than keeping a bot-polled app awake a while longer.

import (
	"net/http"
	"strings"
)

// ActivityClass is how a request affects an app's sleep state.
type ActivityClass int

const (
	// ActivityNormal is real traffic: wake the app, reset the idle clock.
	ActivityNormal ActivityClass = iota
	// ActivityProbe is monitoring traffic: never wakes, never resets the clock.
	ActivityProbe
	// ActivityPin is a long-lived connection: forbids sleep while it is open.
	ActivityPin
	// ActivityCrawl is a search-engine crawler on a real page: wakes the app
	// and holds it for the request, never resets the clock.
	ActivityCrawl
)

func (a ActivityClass) String() string {
	switch a {
	case ActivityProbe:
		return "probe"
	case ActivityPin:
		return "pin"
	case ActivityCrawl:
		return "crawl"
	default:
		return "normal"
	}
}

// probePaths are the conventional health endpoints. Matched exactly (after
// trimming a trailing slash) so an app's real "/healthcheck-dashboard" page
// isn't silently swallowed.
var probePaths = map[string]bool{
	"/health":             true,
	"/healthz":            true,
	"/_health":            true,
	"/healthcheck":        true,
	"/livez":              true,
	"/readyz":             true,
	"/ping":               true,
	"/up":                 true,
	"/.well-known/health": true,
}

// monitorAgents are substrings (lowercased) of User-Agents belonging to uptime
// monitors. They only care about the status code, so a sleeping app may answer
// them with a synthetic 200 on any path.
var monitorAgents = []string{
	"uptimerobot",
	"pingdom",
	"statuscake",
	"betteruptime",
	"better-uptime",
	"uptime-kuma",
	"hetrixtool",
	"site24x7",
	"datadog",
	"newrelic",
	"prometheus",
	"blackbox_exporter",
	"kube-probe",
}

// searchAgents are search-engine crawlers. Whatever they receive may be indexed
// as the page, so on a real path they get the real page — see ActivityCrawl.
var searchAgents = []string{
	"googlebot",
	"bingbot",
	"yandexbot",
}

// botAgents are SEO tools and internet scanners: never worth a wake, and not
// monitors either, so a sleeping app tells them to come back later (503)
// rather than claiming a page says "ok".
var botAgents = []string{
	"ahrefsbot",
	"semrushbot",
	"censys",
	"shodan",
	"zgrab",
	"masscan",
}

// ClassifyActivity assigns a request its activity class.
func ClassifyActivity(r *http.Request) ActivityClass {
	// Long-lived connections first — a WebSocket handshake is a GET, and an SSE
	// request could otherwise be mistaken for an ordinary one.
	if isWebSocketUpgrade(r) || isEventStream(r) {
		return ActivityPin
	}

	if isHealthShaped(r) {
		return ActivityProbe
	}

	ua := r.Header.Get("User-Agent")
	if uaContains(ua, searchAgents) {
		return ActivityCrawl
	}
	if uaContains(ua, monitorAgents) || uaContains(ua, botAgents) {
		return ActivityProbe
	}

	// Cloudflare's bot score: 1–30 is "almost certainly automated". Present only
	// when the zone has Bot Management, so its absence means nothing.
	if score := r.Header.Get("Cf-Bot-Score"); score != "" {
		if n := atoiSafe(score); n > 0 && n <= 30 {
			return ActivityProbe
		}
	}

	return ActivityNormal
}

// isHealthShaped reports a bare liveness poke (HEAD / or OPTIONS /) or a GET/HEAD
// to a conventional health endpoint. A POST to /health is somebody's API.
func isHealthShaped(r *http.Request) bool {
	path := normalizePath(r.URL.Path)
	if (r.Method == http.MethodHead || r.Method == http.MethodOptions) && path == "/" {
		return true
	}
	return (r.Method == http.MethodGet || r.Method == http.MethodHead) && probePaths[path]
}

// SynthesizeForProbe reports whether a probe to a sleeping app may be answered
// with a synthetic 200: health-shaped requests and uptime monitors, which only
// read the status. Other probes (SEO tools, scanners, low bot scores on a real
// page) would record "ok" as the page, so they get a 503 instead.
func SynthesizeForProbe(r *http.Request) bool {
	return isHealthShaped(r) || uaContains(r.Header.Get("User-Agent"), monitorAgents)
}

// normalizePath lowercases and strips a trailing slash so "/Health/" matches.
func normalizePath(p string) string {
	p = strings.ToLower(p)
	if len(p) > 1 {
		p = strings.TrimSuffix(p, "/")
	}
	if p == "" {
		return "/"
	}
	return p
}

func uaContains(ua string, agents []string) bool {
	if ua == "" {
		return false
	}
	ua = strings.ToLower(ua)
	for _, a := range agents {
		if strings.Contains(ua, a) {
			return true
		}
	}
	return false
}

// isWebSocketUpgrade reports a WebSocket handshake. Connection is a
// comma-separated list and both header values are case-insensitive.
func isWebSocketUpgrade(r *http.Request) bool {
	if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		return false
	}
	for _, tok := range strings.Split(r.Header.Get("Connection"), ",") {
		if strings.EqualFold(strings.TrimSpace(tok), "upgrade") {
			return true
		}
	}
	return false
}

// isEventStream reports an SSE request (Accept: text/event-stream).
func isEventStream(r *http.Request) bool {
	for _, v := range strings.Split(r.Header.Get("Accept"), ",") {
		if strings.EqualFold(strings.TrimSpace(strings.SplitN(v, ";", 2)[0]), "text/event-stream") {
			return true
		}
	}
	return false
}

func atoiSafe(s string) int {
	n := 0
	for _, c := range strings.TrimSpace(s) {
		if c < '0' || c > '9' {
			return -1
		}
		n = n*10 + int(c-'0')
		if n > 1_000_000 {
			return -1
		}
	}
	return n
}

// writeSyntheticHealth answers a probe on behalf of a sleeping app.
//
// This closes the loop that otherwise makes scale-to-zero unusable: an external
// monitor polls a sleeping app, the poll wakes it, the app is never idle long
// enough to sleep again. Answering at the edge keeps the monitor green and the
// app asleep. The header makes the substitution auditable rather than a lie.
func writeSyntheticHealth(w http.ResponseWriter, appID string) {
	h := w.Header()
	h.Set("Content-Type", "text/plain; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Tunr-Sleeping", "1")       // app is scaled to zero
	h.Set("X-Tunr-Answered-By", "edge") // this response did not come from the app
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok\n"))
	_ = appID
}

// writeSleepingRetry turns away a non-monitor bot on a real page of a sleeping
// app without waking it. 503 + Retry-After is the status crawlers and SEO tools
// read as "temporarily unavailable, come back", not as the page's content.
func writeSleepingRetry(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Content-Type", "text/plain; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("Retry-After", "3600")
	h.Set("X-Tunr-Sleeping", "1")
	h.Set("X-Tunr-Answered-By", "edge")
	w.WriteHeader(http.StatusServiceUnavailable)
	_, _ = w.Write([]byte("app is asleep\n"))
}
