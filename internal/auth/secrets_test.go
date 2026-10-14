package auth

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestFileStore(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "secrets")
	s := FileStore{Dir: dir}

	if _, err := s.Get("site-token"); !errors.Is(err, ErrSecretNotFound) {
		t.Fatalf("missing secret: %v", err)
	}
	if err := s.Set("site-token", "tunr_site_abc"); err != nil {
		t.Fatal(err)
	}
	if v, err := s.Get("site-token"); err != nil || v != "tunr_site_abc" {
		t.Fatalf("Get = %q, %v", v, err)
	}
	if err := s.Set("site-token", "tunr_site_new"); err != nil {
		t.Fatal(err)
	}
	if v, _ := s.Get("site-token"); v != "tunr_site_new" {
		t.Fatalf("overwrite: %q", v)
	}

	if runtime.GOOS != "windows" {
		st, _ := os.Stat(filepath.Join(dir, "site-token"))
		if st.Mode().Perm() != 0o600 {
			t.Errorf("file mode %o", st.Mode().Perm())
		}
		// A secret someone made world-readable is refused, not silently used.
		_ = os.Chmod(filepath.Join(dir, "site-token"), 0o644)
		if _, err := s.Get("site-token"); err == nil {
			t.Error("world-readable secret was read")
		}
		_ = os.Chmod(filepath.Join(dir, "site-token"), 0o600)
		_ = os.Chmod(dir, 0o755)
		if _, err := s.Get("site-token"); err == nil {
			t.Error("secret in world-readable dir was read")
		}
		_ = os.Chmod(dir, 0o700)
	}

	if err := s.Delete("site-token"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get("site-token"); !errors.Is(err, ErrSecretNotFound) {
		t.Fatalf("after delete: %v", err)
	}
	for _, bad := range []string{"../etc/passwd", "a/b", "", "UPPER"} {
		if err := s.Set(bad, "x"); err == nil {
			t.Errorf("Set(%q) accepted", bad)
		}
	}
}
