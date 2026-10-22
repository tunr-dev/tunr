package relay

import (
	"net"
	"net/http"
	"strings"
)

// systemLabels are the subdomains the platform answers on itself (the CLI's
// relay endpoint, the bridge endpoint, the dashboard, the landing). They can't
// be taken as a tunnel subdomain or an app name, and requests to them reach
// the relay's own routes instead of the tunnel proxy.
var systemLabels = map[string]bool{
	"relay":  true,
	"bridge": true,
	"app":    true,
	"www":    true,
	"api":    true,
}

// IsSystemLabel reports whether label is reserved for the platform.
func IsSystemLabel(label string) bool { return systemLabels[strings.ToLower(label)] }

// TunnelLabel returns the tunnel or app part of host under domain ("myapp"
// for myapp.tunr.sh), or "" when host is the relay's own: the apex, a system
// label, one of extra, or a host outside domain (e.g. the internal
// relay:8080 that Caddy and the dashboard use).
func TunnelLabel(host, domain string, extra ...string) string {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	label, ok := strings.CutSuffix(host, "."+strings.ToLower(domain))
	if !ok || label == "" || IsSystemLabel(label) {
		return ""
	}
	for _, e := range extra {
		if strings.EqualFold(label, e) {
			return ""
		}
	}
	return label
}

// SplitByHost sends tunnel and app hosts to tunnels and everything else to
// own. Without it the relay's own routes (/api/v1/*, /auth/*, /portal/,
// /bridge/*, /v1/apps) would shadow the same paths on every user's tunnel.
func SplitByHost(domain string, own, tunnels http.Handler, extra ...string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if TunnelLabel(r.Host, domain, extra...) != "" {
			tunnels.ServeHTTP(w, r)
			return
		}
		own.ServeHTTP(w, r)
	})
}
