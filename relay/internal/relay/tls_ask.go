package relay

import (
	"net/http"
	"strings"
)

// TLSAskHandler answers Caddy's on-demand TLS "ask" check
// (GET ?domain=<host>): 200 for the relay's own domain and its single-label
// subdomains, 404 for anything else. Without it, a self-hosted Caddy would
// issue a certificate for any hostname pointed at the box.
//
// The production deployment uses a DNS-01 wildcard certificate and never
// calls this; it exists for the self-host compose stack.
func TLSAskHandler(domain string) http.HandlerFunc {
	domain = strings.ToLower(strings.TrimSuffix(domain, "."))
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if !servesHost(domain, r.URL.Query().Get("domain")) {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusOK)
	}
}

func servesHost(domain, host string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if domain == "" || host == "" {
		return false
	}
	if host == domain {
		return true
	}
	label, ok := strings.CutSuffix(host, "."+domain)
	if !ok || label == "" || len(label) > 63 || strings.Contains(label, ".") {
		return false
	}
	for _, c := range label {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
			return false
		}
	}
	return true
}
