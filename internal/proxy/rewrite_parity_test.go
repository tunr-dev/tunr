package proxy

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// What the dev server receives through LocalProxy. The proxy moved from
// ReverseProxy.Director to Rewrite; these cases pin the Director-era
// behaviour so the two stay indistinguishable to the app.
type seenRequest struct {
	upstream, host, uri, fwd, fwdHost, fwdProto, forwarded string
	ua                                                     []string
}

func recordingUpstream(t *testing.T, name string, seen *seenRequest) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*seen = seenRequest{
			upstream:  name,
			host:      r.Host,
			uri:       r.RequestURI,
			fwd:       r.Header.Get("X-Forwarded-For"),
			fwdHost:   r.Header.Get("X-Forwarded-Host"),
			fwdProto:  r.Header.Get("X-Forwarded-Proto"),
			forwarded: r.Header.Get("Forwarded"),
			ua:        r.Header.Values("User-Agent"),
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestLocalProxy_UpstreamSeesSameRequestAsDirector(t *testing.T) {
	var seen seenRequest
	app := recordingUpstream(t, "app", &seen)
	api := recordingUpstream(t, "api", &seen)
	appPort, apiPort := mustPortFromURL(t, app.URL), mustPortFromURL(t, api.URL)

	p, err := NewLocalProxy(appPort, map[string]int{"/": appPort, "/api": apiPort})
	if err != nil {
		t.Fatal(err)
	}
	p.BuildMiddlewareChain()

	relayHeaders := func(r *http.Request) {
		// What the relay stamps on every request (relay/internal/relay/proxy.go).
		r.Header.Set("X-Forwarded-For", "203.0.113.7")
		r.Header.Set("X-Forwarded-Host", "demo.tunr.sh")
		r.Header.Set("X-Forwarded-Proto", "https")
		r.Header.Set("Forwarded", "for=203.0.113.7")
		r.Header.Set("User-Agent", "Mozilla/5.0")
	}

	cases := []struct {
		name   string
		build  func() *http.Request
		expect seenRequest
	}{
		{
			// forwardToLocal builds requests with http.NewRequest: no RemoteAddr.
			name: "tunnel request keeps the relay's forwarding headers and Host",
			build: func() *http.Request {
				r, _ := http.NewRequest(http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/page?x=1", appPort), nil)
				relayHeaders(r)
				return r
			},
			expect: seenRequest{
				upstream: "app", host: fmt.Sprintf("127.0.0.1:%d", appPort), uri: "/page?x=1",
				fwd: "203.0.113.7", fwdHost: "demo.tunr.sh", fwdProto: "https", forwarded: "for=203.0.113.7",
				ua: []string{"Mozilla/5.0"},
			},
		},
		{
			name: "path route goes to its port, Host untouched",
			build: func() *http.Request {
				r, _ := http.NewRequest(http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/api/users", appPort), nil)
				relayHeaders(r)
				return r
			},
			expect: seenRequest{
				upstream: "api", host: fmt.Sprintf("127.0.0.1:%d", appPort), uri: "/api/users",
				fwd: "203.0.113.7", fwdHost: "demo.tunr.sh", fwdProto: "https", forwarded: "for=203.0.113.7",
				ua: []string{"Mozilla/5.0"},
			},
		},
		{
			name: "a peer address is appended to X-Forwarded-For",
			build: func() *http.Request {
				r := httptest.NewRequest(http.MethodGet, "http://shared.example/p", nil)
				r.RemoteAddr = "192.0.2.10:5555"
				r.Header.Set("X-Forwarded-For", "203.0.113.7")
				return r
			},
			expect: seenRequest{upstream: "app", host: "shared.example", uri: "/p", fwd: "203.0.113.7, 192.0.2.10", ua: []string{""}},
		},
		{
			name: "no User-Agent means none is invented",
			build: func() *http.Request {
				r, _ := http.NewRequest(http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/", appPort), nil)
				return r
			},
			expect: seenRequest{upstream: "app", host: fmt.Sprintf("127.0.0.1:%d", appPort), uri: "/", ua: []string{""}},
		},
		{
			// Rewrite drops query params that don't parse; Director passed them on.
			name: "unparsable query reaches the app unchanged",
			build: func() *http.Request {
				r, _ := http.NewRequest(http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/legacy?a=1;b=2&c=%%zz", appPort), nil)
				return r
			},
			expect: seenRequest{upstream: "app", host: fmt.Sprintf("127.0.0.1:%d", appPort), uri: "/legacy?a=1;b=2&c=%zz", ua: []string{""}},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			seen = seenRequest{}
			rec := httptest.NewRecorder()
			p.ServeHTTP(rec, tc.build())
			if rec.Code != http.StatusOK {
				t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
			}
			// Go's client sends an empty User-Agent as no header at all.
			if len(seen.ua) == 0 {
				seen.ua = []string{""}
			}
			if fmt.Sprint(seen) != fmt.Sprint(tc.expect) {
				t.Fatalf("upstream saw\n  %+v\nwant\n  %+v", seen, tc.expect)
			}
		})
	}
}
