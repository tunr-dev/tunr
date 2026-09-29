package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func TestRawWSEndpoint(t *testing.T) {
	cases := map[string]string{
		"https://ab12cd34.tunr.sh":   "wss://ab12cd34.tunr.sh/tunnel/tcp?subdomain=ab12cd34",
		"https://x.example.com:8443": "wss://x.example.com:8443/tunnel/tcp?subdomain=x",
		"":                           "",
		"not a url":                  "",
	}
	for in, want := range cases {
		if got := rawWSEndpoint(in); got != want {
			t.Errorf("rawWSEndpoint(%q) = %q, want %q", in, got, want)
		}
	}
}

// --allow-ip used to be accepted on raw tunnels and silently ignored, leaving
// the port open to everyone. It must fail before any tunnel is started.
func TestRawTunnels_RejectAllowIP(t *testing.T) {
	for _, mk := range []func() *cobra.Command{newTCPCmd, newUDPCmd, newTLSCmd} {
		cmd := mk()
		cmd.SetArgs([]string{"--port", "5432", "--allow-ip", "10.0.0.0/8"})
		cmd.SilenceUsage = true
		cmd.SilenceErrors = true
		if err := cmd.Execute(); !errors.Is(err, errRawAllowIP) {
			t.Errorf("tunr %s --allow-ip: expected errRawAllowIP, got %v", cmd.Name(), err)
		}
	}
}

func TestRawTunnels_HelpMakesNoE2EClaim(t *testing.T) {
	for _, mk := range []func() *cobra.Command{newTCPCmd, newUDPCmd, newTLSCmd} {
		cmd := mk()
		help := strings.ToLower(cmd.Short + " " + cmd.Long)
		for _, bad := range []string{"cannot read", "end-to-end encryption", "zero-trust", "compliance", "sni-based routing"} {
			if strings.Contains(help, bad) {
				t.Errorf("tunr %s help claims %q", cmd.Name(), bad)
			}
		}
		if !strings.Contains(help, "experimental") {
			t.Errorf("tunr %s help should say it's experimental", cmd.Name())
		}
	}
}
