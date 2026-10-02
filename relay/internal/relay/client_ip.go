package relay

import (
	"net"
	"net/http"
	"net/netip"
	"strings"
)

// ClientIP returns the address of the client that sent r. Rate limits and
// the X-Forwarded-For the CLI's --allow-ip checks are built on it, so a client
// must not be able to choose it.
//
// Forwarding headers are believed only when the TCP peer is a private or
// loopback address, i.e. our own reverse proxy (Caddy on the docker network
// in prod). Then the rightmost X-Forwarded-For entry wins: it is the one the
// nearest proxy set or appended, and anything to its left came from the
// client. A peer on a public address is talking to us directly, so its
// headers are ignored and its own address is the answer.
//
// Fly-Client-IP and CF-Connecting-IP are never read: anyone can send them.
// Caddy resolves Cloudflare's header itself, only for Cloudflare's ranges,
// and puts the result in X-Forwarded-For.
func ClientIP(r *http.Request) string {
	peer := r.RemoteAddr
	if host, _, err := net.SplitHostPort(peer); err == nil {
		peer = host
	}
	addr, err := netip.ParseAddr(peer)
	if err != nil || !(addr.IsLoopback() || addr.IsPrivate()) {
		return peer
	}
	if fwd := rightmostForwardedFor(r.Header.Values("X-Forwarded-For")); fwd != "" {
		return fwd
	}
	return peer
}

func rightmostForwardedFor(values []string) string {
	for i := len(values) - 1; i >= 0; i-- {
		parts := strings.Split(values[i], ",")
		for j := len(parts) - 1; j >= 0; j-- {
			p := strings.TrimSpace(parts[j])
			if p == "" {
				continue
			}
			if addr, err := netip.ParseAddr(p); err == nil {
				return addr.String()
			}
			return "" // garbage from the nearest hop: don't guess further left
		}
	}
	return ""
}
