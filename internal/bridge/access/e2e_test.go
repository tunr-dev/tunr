package access

import (
	"crypto/tls"
	"io"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tunr-dev/tunr/internal/auth"
	"github.com/tunr-dev/tunr/internal/bridge/connector"
)

// The whole E2E chain without a relay: connector cert from the secret store,
// the connector's TLS termination in front of a target, and the agent's
// pinned client — then the same with the wrong pin.
func TestEndToEndTLSPinning(t *testing.T) {
	store := auth.FileStore{Dir: filepath.Join(t.TempDir(), "secrets")}
	cert, fp, err := connector.LoadOrCreateSiteTLS(store, "hq-site")
	if err != nil {
		t.Fatal(err)
	}
	_, fp2, _ := connector.LoadOrCreateSiteTLS(store, "hq-site")
	if fp != fp2 || len(fp) != 64 {
		t.Fatalf("fingerprint not stable across loads: %s vs %s", fp, fp2)
	}

	l, _ := net.Listen("tcp", "127.0.0.1:0")
	defer l.Close()
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				b := make([]byte, 64)
				n, _ := c.Read(b)
				_, _ = c.Write([]byte(strings.ToUpper(string(b[:n]))))
			}()
		}
	}()

	open := func(pin string) (string, error) {
		target, err := net.Dial("tcp", l.Addr().String())
		if err != nil {
			return "", err
		}
		relaySide := connector.WrapTLSForTest(target, cert)
		tc := tls.Client(relaySide, PinnedTLS(pin))
		defer tc.Close()
		_ = tc.SetDeadline(time.Now().Add(3 * time.Second))
		if err := tc.Handshake(); err != nil {
			return "", err
		}
		if tc.ConnectionState().Version != tls.VersionTLS13 {
			t.Errorf("TLS version %x", tc.ConnectionState().Version)
		}
		_, _ = tc.Write([]byte("select 1"))
		b, err := io.ReadAll(io.LimitReader(tc, 8))
		return string(b), err
	}
	if got, err := open(fp); err != nil || got != "SELECT 1" {
		t.Fatalf("pinned session: %q %v", got, err)
	}
	if _, err := open(strings.Repeat("0", 64)); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("wrong pin accepted: %v", err)
	}
}
