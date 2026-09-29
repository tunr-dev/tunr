package main

import (
	"bytes"
	"os"
	"testing"

	"github.com/tunr-dev/tunr/internal/logger"
)

func TestVersionIsSet(t *testing.T) {
	if Version == "" {
		t.Fatal("Version should not be empty")
	}
}

func TestRootCommandExists(t *testing.T) {
	if rootCmd == nil {
		t.Fatal("rootCmd should not be nil")
	}

	if rootCmd.Use != "tunr" {
		t.Errorf("expected 'tunr', got '%s'", rootCmd.Use)
	}
}

func TestRootHasSubcommands(t *testing.T) {
	expected := []string{
		"share", "start", "stop", "status", "logs",
		"doctor", "login", "logout", "version",
		"open", "replay", "mcp", "config",
		"update", "uninstall",
	}

	cmds := rootCmd.Commands()
	names := make(map[string]bool)
	for _, c := range cmds {
		names[c.Name()] = true
	}

	for _, exp := range expected {
		if !names[exp] {
			t.Errorf("missing subcommand: %s", exp)
		}
	}
}

// --json output is meant to be piped into jq or a script, so progress lines must not
// share stdout with it.
func TestJSONFlagMovesInfoOffStdout(t *testing.T) {
	var buf bytes.Buffer
	logger.SetInfoOutput(&buf)
	t.Cleanup(func() { logger.SetInfoOutput(os.Stdout) })

	cmd := newTCPCmd()
	if err := cmd.Flags().Set("json", "true"); err != nil {
		t.Fatal(err)
	}
	rootCmd.PersistentPreRun(cmd, nil)
	logger.Info("Starting TCP tunnel...")

	if buf.Len() != 0 {
		t.Fatalf("INFO still written to the stdout writer under --json: %q", buf.String())
	}
}
