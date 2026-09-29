package proxy

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/tunr-dev/tunr/internal/logger"
)

func TestCleanRemote(t *testing.T) {
	cases := map[string]struct{ in, want string }{
		"plain":         {"Make the logo bigger 🙏", "Make the logo bigger 🙏"},
		"ansi color":    {"\x1b[31mred\x1b[0m", "[31mred[0m"},
		"osc title":     {"\x1b]0;pwned\x07hi", "]0;pwnedhi"},
		"c1 csi":        {"a\u009b2Jb", "a2Jb"},
		"clear + bell":  {"\x1b[2J\x1b[H\a", "[2J[H"},
		"newlines":      {"line1\nline2\r\n\tx", "line1 line2   x"},
		"bidi override": {"abc‮dcba", "abcdcba"},
		"invalid utf8":  {"ok\xffok", "okok"},
		"del":           {"a\x7fb", "ab"},
	}
	for name, c := range cases {
		if got := cleanRemote(c.in); got != c.want {
			t.Errorf("%s: cleanRemote(%q) = %q, want %q", name, c.in, got, c.want)
		}
	}

	long := cleanRemote(strings.Repeat("x", 5000))
	if n := len([]rune(long)); n != maxRemoteField+1 || !strings.HasSuffix(long, "…") {
		t.Errorf("expected %d runes ending in …, got %d", maxRemoteField+1, n)
	}
}

func newWidgetProxy(t *testing.T) *LocalProxy {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	t.Cleanup(upstream.Close)
	p, err := NewLocalProxy(mustPortFromURL(t, upstream.URL), nil)
	if err != nil {
		t.Fatalf("new local proxy: %v", err)
	}
	p.InjectWidget = true
	p.BuildMiddlewareChain()
	return p
}

func captureInfo(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	logger.SetInfoOutput(&buf)
	t.Cleanup(func() { logger.SetInfoOutput(os.Stdout) })
	return &buf
}

func TestFeedbackEndpoint_ScrubsEscapeSequences(t *testing.T) {
	p := newWidgetProxy(t)
	out := captureInfo(t)

	for _, path := range []string{"/__tunr/feedback", "/__tunr/error"} {
		body := `{"message":"\u001b]0;pwned\u0007\u001b[2Jhello","url":"\u001b[31m/x","type":"\u001b[1mE","source":"\u001b[Hs.js"}`
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: expected 200, got %d", path, rec.Code)
		}
	}

	// The logger's own styling uses ESC, so check that no escape sequence the
	// visitor sent survived rather than that ESC is absent altogether.
	for _, bad := range []string{"\x1b]0;", "\x07", "\x1b[2J", "\x1b[H", "\x1b[31m/x", "\x1b[1mE"} {
		if strings.Contains(out.String(), bad) {
			t.Fatalf("visitor escape sequence %q reached the terminal:\n%q", bad, out.String())
		}
	}
	if !strings.Contains(out.String(), "hello") {
		t.Fatalf("expected the message text to still be printed, got:\n%q", out.String())
	}
}

func TestFeedbackEndpoint_RejectsOversizedBody(t *testing.T) {
	p := newWidgetProxy(t)
	captureInfo(t)

	body := `{"message":"` + strings.Repeat("a", maxRemotePayload) + `"}`
	req := httptest.NewRequest(http.MethodPost, "/__tunr/feedback", strings.NewReader(body))
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for a body over %d bytes, got %d", maxRemotePayload, rec.Code)
	}
}
