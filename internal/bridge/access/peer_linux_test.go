//go:build linux

package access

import (
	"net"
	"testing"
)

func TestPeerAllowedSameUser(t *testing.T) {
	for _, network := range []string{"tcp4", "tcp6"} {
		l, err := net.Listen(network, map[string]string{"tcp4": "127.0.0.1:0", "tcp6": "[::1]:0"}[network])
		if err != nil {
			t.Logf("%s: %v", network, err)
			continue
		}
		go func() {
			c, err := net.Dial(network, l.Addr().String())
			if err == nil {
				defer c.Close()
				var b [1]byte
				_, _ = c.Read(b[:])
			}
		}()
		srv, err := l.Accept()
		if err != nil {
			t.Fatal(err)
		}
		ok, err := peerAllowed(srv)
		if !ok || err != nil {
			t.Errorf("%s: own process refused: %v", network, err)
		}
		srv.Close()
		l.Close()
	}
}
