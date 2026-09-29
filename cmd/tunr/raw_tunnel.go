package main

import (
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/tunr-dev/tunr/internal/term"
)

// Raw tunnels (tcp, udp, tls) are only reachable through the relay's WebSocket
// bridge at wss://<sub>.<domain>/tunnel/tcp. The relay has no raw TCP/UDP port
// and no SNI passthrough, so ordinary clients (psql, ssh, a TLS client) can't
// connect to them directly, and the relay can't enforce an IP allowlist on
// them. Help text and output must say exactly that — nothing more.

const rawTunnelLimits = `Experimental: the relay exposes raw tunnels only as a WebSocket stream at
wss://<subdomain>.tunr.sh/tunnel/tcp. Clients such as psql, ssh or a browser's
TLS stack can't connect to it directly yet; you need a client that speaks
that WebSocket (binary frames carry the raw bytes).`

var errRawAllowIP = errors.New("--allow-ip is not supported for TCP/UDP/TLS tunnels yet: the relay can't enforce an allowlist on raw tunnels")

func rejectRawAllowIP(ips []string) error {
	if len(ips) > 0 {
		return errRawAllowIP
	}
	return nil
}

// rawWSEndpoint turns a tunnel's public URL into the WebSocket endpoint that
// actually reaches it, e.g. https://ab12.tunr.sh → wss://ab12.tunr.sh/tunnel/tcp?subdomain=ab12.
func rawWSEndpoint(publicURL string) string {
	u, err := url.Parse(publicURL)
	if err != nil || u.Host == "" {
		return ""
	}
	sub, _, _ := strings.Cut(u.Hostname(), ".")
	return fmt.Sprintf("wss://%s/tunnel/tcp?subdomain=%s", u.Host, url.QueryEscape(sub))
}

func printRawTunnelNote(publicURL string) {
	if ws := rawWSEndpoint(publicURL); ws != "" {
		term.Dim.Println("  Reachable only over WebSocket (experimental):")
		term.Cyan.Println("  " + ws)
		term.Dim.Println("  psql, ssh and other native clients can't connect to it directly yet.")
	}
}
