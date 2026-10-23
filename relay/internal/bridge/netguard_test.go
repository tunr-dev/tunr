package bridge

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
)

func TestIsPublicAddr(t *testing.T) {
	for _, s := range []string{"127.0.0.1", "10.1.2.3", "172.16.0.1", "192.168.1.1", "169.254.169.254",
		"100.64.0.1", "0.0.0.0", "::1", "fe80::1", "fd00::1", "::ffff:127.0.0.1", "64:ff9b::a00:1"} {
		if isPublicAddr(netip.MustParseAddr(s).Unmap()) {
			t.Errorf("%s treated as public", s)
		}
	}
	for _, s := range []string{"1.1.1.1", "8.8.8.8", "2606:4700:4700::1111"} {
		if !isPublicAddr(netip.MustParseAddr(s)) {
			t.Errorf("%s treated as non-public", s)
		}
	}
}

func TestWebhookClientRefusesLoopback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("loopback server was reached")
	}))
	defer srv.Close()
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, srv.URL, nil)
	if resp, err := webhookClient.Do(req); err == nil {
		resp.Body.Close()
		t.Fatal("webhook client dialed a loopback address")
	}
}
