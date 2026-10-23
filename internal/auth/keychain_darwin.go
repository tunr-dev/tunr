package auth

import (
	"encoding/hex"
	"fmt"
	"os/exec"
	"strings"
)

// macKeychainAdd writes a generic password (updating it in place). The value
// goes to `security -i` on stdin, hex-encoded with -X, so it never appears in
// any process's argv and needs no quoting. security's interactive line buffer
// is about 4 KB; larger values fall back to -w on the command line.
func macKeychainAdd(service, account, value string) ([]byte, error) {
	line := fmt.Sprintf("add-generic-password -s %s -a %s -X %s -U\n", service, account, hex.EncodeToString([]byte(value)))
	if len(line) < 4000 && !strings.ContainsAny(service+account, " \t\"'\\\n") {
		cmd := exec.Command("security", "-i")
		cmd.Stdin = strings.NewReader(line)
		out, err := cmd.CombinedOutput()
		if err == nil && !strings.Contains(string(out), "returned") && !strings.Contains(string(out), "unknown command") {
			return out, nil
		}
		if err == nil {
			err = fmt.Errorf("security -i failed")
		}
		return out, err
	}
	return exec.Command("security", "add-generic-password", "-s", service, "-a", account, "-w", value, "-U").CombinedOutput()
}
