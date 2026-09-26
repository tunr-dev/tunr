package main

import (
	"fmt"
	"os"
	"runtime/debug"
	"strings"

	"github.com/tunr-dev/tunr/internal/tunnel"
)

var Version = "dev"
var BuildDate = ""
var Commit = ""

func init() {
	// `go install …@v0.6.1` doesn't pass -ldflags, but the module version is
	// recorded in the binary — use it so `tunr version` and `tunr update` work.
	if Version == "dev" {
		if bi, ok := debug.ReadBuildInfo(); ok && strings.HasPrefix(bi.Main.Version, "v") {
			Version = strings.TrimPrefix(bi.Main.Version, "v")
		}
	}
	tunnel.Version = Version
}

func main() {
	defer func() {
		if r := recover(); r != nil {
			fmt.Fprintf(os.Stderr, "\nUnexpected error: %v\n", r)
			fmt.Fprintln(os.Stderr, "Please report: https://github.com/tunr-dev/tunr/issues")
			os.Exit(1)
		}
	}()

	if err := Execute(); err != nil {
		os.Exit(1)
	}
}
