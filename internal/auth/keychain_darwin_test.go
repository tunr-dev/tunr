package auth

import (
	"os"
	"os/exec"
	"testing"
)

// Writes and reads a throwaway login-keychain item; opt in with TUNR_TEST_KEYCHAIN=1.
func TestMacKeychainAddViaStdin(t *testing.T) {
	if os.Getenv("TUNR_TEST_KEYCHAIN") != "1" {
		t.Skip("set TUNR_TEST_KEYCHAIN=1 to touch the login keychain")
	}
	const svc, acct = "tunr-selftest", "probe"
	defer exec.Command("security", "delete-generic-password", "-s", svc, "-a", acct).Run()
	for _, v := range []string{`quote" back\slash space`, "x"} {
		if out, err := macKeychainAdd(svc, acct, v); err != nil {
			t.Fatalf("add: %v %s", err, out)
		}
		got, err := exec.Command("security", "find-generic-password", "-s", svc, "-a", acct, "-w").Output()
		if err != nil || string(got) != v+"\n" {
			t.Fatalf("read back %q, want %q (%v)", got, v, err)
		}
	}
}
