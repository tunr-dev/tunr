package relay

import (
	"net/http"
	"testing"
)

func TestClientIP(t *testing.T) {
	cases := []struct {
		name   string
		remote string
		hdr    http.Header
		want   string
	}{
		{"direct public peer ignores every header", "198.51.100.4:5555", http.Header{
			"X-Forwarded-For":  {"203.0.113.5"},
			"Fly-Client-Ip":    {"203.0.113.5"},
			"Cf-Connecting-Ip": {"203.0.113.5"},
		}, "198.51.100.4"},
		{"behind caddy: forwarded-for is used", "172.18.0.5:40000", http.Header{
			"X-Forwarded-For": {"198.51.100.4"},
		}, "198.51.100.4"},
		{"behind caddy: rightmost entry, not the client's prefix", "172.18.0.5:40000", http.Header{
			"X-Forwarded-For": {"203.0.113.5, 198.51.100.4"},
		}, "198.51.100.4"},
		{"behind caddy: repeated headers, last one wins", "10.0.0.2:1", http.Header{
			"X-Forwarded-For": {"203.0.113.5", "198.51.100.4"},
		}, "198.51.100.4"},
		{"behind caddy: spoof headers ignored", "127.0.0.1:1", http.Header{
			"X-Forwarded-For":  {"198.51.100.4"},
			"Fly-Client-Ip":    {"203.0.113.5"},
			"Cf-Connecting-Ip": {"203.0.113.5"},
		}, "198.51.100.4"},
		{"behind caddy: no header falls back to peer", "172.18.0.5:40000", nil, "172.18.0.5"},
		{"behind caddy: garbage rightmost is not skipped", "172.18.0.5:40000", http.Header{
			"X-Forwarded-For": {"203.0.113.5, not-an-ip"},
		}, "172.18.0.5"},
		{"ipv6 peer", "[2001:db8::1]:443", http.Header{"X-Forwarded-For": {"203.0.113.5"}}, "2001:db8::1"},
		{"ipv6 forwarded", "[::1]:1", http.Header{"X-Forwarded-For": {"2001:db8::7"}}, "2001:db8::7"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &http.Request{RemoteAddr: tc.remote, Header: tc.hdr}
			if r.Header == nil {
				r.Header = http.Header{}
			}
			if got := ClientIP(r); got != tc.want {
				t.Fatalf("ClientIP = %q, want %q", got, tc.want)
			}
		})
	}
}
