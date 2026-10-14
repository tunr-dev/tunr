package auth

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
)

// secrets.go — named secrets for the bridge connector (site token, database
// credentials), separate from the user's login token.
//
// Two stores:
//
//   - KeychainStore: the OS keychain (macOS Keychain, libsecret via
//     secret-tool, Windows DPAPI via Export-Clixml). Default on desktops.
//   - FileStore: one 0600 file per secret in a 0700 directory, refusing to
//     read anything group/world-accessible. NOT encrypted — protection is file
//     ownership. It exists because the connector's main home is a headless
//     Linux server running as a systemd service, where there is no keychain
//     session; it is only used when chosen explicitly (secret_store: "file").
//
// SECURITY: never log a secret value or pass one to an error message.

// ErrSecretNotFound is returned when a secret does not exist.
var ErrSecretNotFound = errors.New("secret not found")

// SecretStore stores named secrets.
type SecretStore interface {
	Get(name string) (string, error)
	Set(name, value string) error
	Delete(name string) error
}

var secretNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

func checkSecretName(name string) error {
	if !secretNameRe.MatchString(name) {
		return fmt.Errorf("invalid secret name %q", name)
	}
	return nil
}

const maxSecretLen = 8 << 10

// ── Keychain ─────────────────────────────────────────────────────────────────

// KeychainStore keeps secrets in the OS keychain under service "tunr.sh-connector".
type KeychainStore struct{}

const connectorKeychainService = "tunr.sh-connector"

func (KeychainStore) Set(name, value string) error {
	if err := checkSecretName(name); err != nil {
		return err
	}
	if value == "" || len(value) > maxSecretLen {
		return errors.New("secret must be 1..8192 bytes")
	}
	switch runtime.GOOS {
	case "darwin":
		// -U updates in place. The value is briefly visible in this process's
		// argv (same as StoreToken); `security` offers no stdin form.
		out, err := exec.Command("security", "add-generic-password",
			"-s", connectorKeychainService, "-a", name, "-w", value, "-U").CombinedOutput()
		if err != nil {
			return fmt.Errorf("keychain write failed: %s", sanitizeOutput(string(out)))
		}
		return nil
	case "linux":
		cmd := exec.Command("secret-tool", "store", "--label", "tunr connector: "+name,
			"service", connectorKeychainService, "account", name)
		cmd.Stdin = strings.NewReader(value)
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("secret-tool failed (no keyring session? use secret_store \"file\" on servers): %s",
				sanitizeOutput(string(out)))
		}
		return nil
	case "windows":
		// The value travels in an environment variable, never inside the
		// script text, so quotes in a password can't break out of it.
		path, err := windowsSecretPath(name)
		if err != nil {
			return err
		}
		cmd := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command",
			`$s = ConvertTo-SecureString $env:TUNR_SECRET_VALUE -AsPlainText -Force; `+
				`New-Item -ItemType Directory -Force -Path (Split-Path $env:TUNR_SECRET_PATH) | Out-Null; `+
				`New-Object PSCredential('tunr', $s) | Export-Clixml -Path $env:TUNR_SECRET_PATH`)
		cmd.Env = append(os.Environ(), "TUNR_SECRET_VALUE="+value, "TUNR_SECRET_PATH="+path)
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("credential store write failed: %s", sanitizeOutput(string(out)))
		}
		return nil
	}
	return fmt.Errorf("no keychain on %s; use secret_store \"file\"", runtime.GOOS)
}

func (KeychainStore) Get(name string) (string, error) {
	if err := checkSecretName(name); err != nil {
		return "", err
	}
	var out []byte
	var err error
	switch runtime.GOOS {
	case "darwin":
		out, err = exec.Command("security", "find-generic-password",
			"-s", connectorKeychainService, "-a", name, "-w").Output()
	case "linux":
		out, err = exec.Command("secret-tool", "lookup",
			"service", connectorKeychainService, "account", name).Output()
	case "windows":
		path, perr := windowsSecretPath(name)
		if perr != nil {
			return "", perr
		}
		cmd := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command",
			`(Import-Clixml -Path $env:TUNR_SECRET_PATH).GetNetworkCredential().Password`)
		cmd.Env = append(os.Environ(), "TUNR_SECRET_PATH="+path)
		out, err = cmd.Output()
	default:
		return "", ErrSecretNotFound
	}
	v := strings.TrimRight(string(out), "\r\n")
	if err != nil || v == "" {
		return "", ErrSecretNotFound
	}
	return v, nil
}

func (KeychainStore) Delete(name string) error {
	if err := checkSecretName(name); err != nil {
		return err
	}
	switch runtime.GOOS {
	case "darwin":
		_ = exec.Command("security", "delete-generic-password",
			"-s", connectorKeychainService, "-a", name).Run()
	case "linux":
		_ = exec.Command("secret-tool", "clear", "service", connectorKeychainService, "account", name).Run()
	case "windows":
		if path, err := windowsSecretPath(name); err == nil {
			_ = os.Remove(path)
		}
	}
	return nil
}

func windowsSecretPath(name string) (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "tunr", "connector", name+".xml"), nil
}

// ── File ─────────────────────────────────────────────────────────────────────

// FileStore keeps each secret in Dir/<name>, mode 0600, Dir mode 0700.
type FileStore struct{ Dir string }

func (f FileStore) path(name string) (string, error) {
	if err := checkSecretName(name); err != nil {
		return "", err
	}
	if f.Dir == "" {
		return "", errors.New("file secret store: no directory")
	}
	return filepath.Join(f.Dir, name), nil
}

func (f FileStore) Set(name, value string) error {
	p, err := f.path(name)
	if err != nil {
		return err
	}
	if value == "" || len(value) > maxSecretLen {
		return errors.New("secret must be 1..8192 bytes")
	}
	if err := os.MkdirAll(f.Dir, 0o700); err != nil {
		return err
	}
	if err := checkPrivate(f.Dir, true); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(f.Dir, ".tmp-"+name+"-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.WriteString(value); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), p)
}

func (f FileStore) Get(name string) (string, error) {
	p, err := f.path(name)
	if err != nil {
		return "", err
	}
	if err := checkPrivate(f.Dir, true); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", ErrSecretNotFound
		}
		return "", err
	}
	if err := checkPrivate(p, false); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", ErrSecretNotFound
		}
		return "", err
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return "", err
	}
	if len(b) == 0 {
		return "", ErrSecretNotFound
	}
	return string(b), nil
}

func (f FileStore) Delete(name string) error {
	p, err := f.path(name)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// checkPrivate refuses paths readable by group/others (POSIX only; Windows
// ACLs are not mode bits, so the check is skipped there).
func checkPrivate(p string, dir bool) error {
	st, err := os.Stat(p)
	if err != nil {
		return err
	}
	if dir != st.IsDir() {
		return fmt.Errorf("%s: unexpected file type", p)
	}
	if runtime.GOOS != "windows" && st.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("%s is accessible by other users (mode %o); run: chmod %s %s",
			p, st.Mode().Perm(), map[bool]string{true: "700", false: "600"}[dir], p)
	}
	return nil
}
