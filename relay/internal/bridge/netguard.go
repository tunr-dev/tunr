package bridge

import (
	"errors"
	"net"
	"net/http"
	"net/netip"
	"syscall"
	"time"
)

// netguard.go — outbound HTTP for org-configured URLs (webhooks). The URL is
// chosen by a tenant, so the relay only dials public addresses and never
// follows redirects; the check runs on the resolved address at dial time, so
// DNS answers can't route around it.

var errNonPublicAddr = errors.New("webhook target is not a public address")

func publicOnly(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return err
	}
	if !isPublicAddr(ip.Unmap()) {
		return errNonPublicAddr
	}
	return nil
}

func isPublicAddr(ip netip.Addr) bool {
	if !ip.IsValid() || ip.IsUnspecified() || ip.IsLoopback() || ip.IsPrivate() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsInterfaceLocalMulticast() || ip.IsMulticast() {
		return false
	}
	for _, p := range nonPublicPrefixes {
		if p.Contains(ip) {
			return false
		}
	}
	return true
}

var nonPublicPrefixes = func() []netip.Prefix {
	var out []netip.Prefix
	for _, s := range []string{
		"0.0.0.0/8", "100.64.0.0/10", "192.0.0.0/24", "192.0.2.0/24", "198.18.0.0/15",
		"198.51.100.0/24", "203.0.113.0/24", "240.0.0.0/4",
		"64:ff9b::/96", "64:ff9b:1::/48", "2001:db8::/32", "2002::/16", "100::/64",
	} {
		out = append(out, netip.MustParsePrefix(s))
	}
	return out
}()

// newPublicClient is the default client for tenant-supplied URLs.
func newPublicClient(timeout time.Duration) *http.Client {
	d := &net.Dialer{Timeout: 5 * time.Second, Control: publicOnly}
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			Proxy:               nil,
			DialContext:         d.DialContext,
			TLSHandshakeTimeout: 5 * time.Second,
			MaxIdleConns:        10,
			IdleConnTimeout:     30 * time.Second,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}
