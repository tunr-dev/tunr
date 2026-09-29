package main

import (
	"fmt"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"
	"github.com/tunr-dev/tunr/internal/auth"
	"github.com/tunr-dev/tunr/internal/config"
	"github.com/tunr-dev/tunr/internal/logger"
	"github.com/tunr-dev/tunr/internal/term"
	"github.com/tunr-dev/tunr/internal/tunnel"
)

func newTLSCmd() *cobra.Command {
	var port int
	var noOpen bool
	var jsonOutput bool
	var qrCode bool
	var allowedIPs []string
	var region string

	cmd := &cobra.Command{
		Use:     "tls",
		Aliases: []string{"tls-proxy", "e2e"},
		Short:   "Expose a local TLS port as a raw tunnel (experimental)",
		Long: `Forward a local TLS port as a raw tunnel — the same transport as 'tunr tcp'.

This is not SNI passthrough: the relay terminates TLS for *.tunr.sh, and a
normal TLS client connecting to the public URL reaches the relay, not your
server. The payload stays encrypted to your server only if the client runs
its own TLS session inside the WebSocket stream.

` + rawTunnelLimits,
		Example: `  tunr tls --port 8443`,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, stop := signal.NotifyContext(cmd.Context(),
				syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
			defer stop()

			if port == 0 {
				return fmt.Errorf("port is required (use --port)")
			}
			if err := rejectRawAllowIP(allowedIPs); err != nil {
				return err
			}

			cfg, err := config.Load()
			if err != nil {
				logger.Warn("Config not found, using defaults")
				cfg = config.DefaultConfig()
			}

			token, _ := auth.GetToken()

			mgr := tunnel.NewManager(relayURL())
			mgr.SetAuthToken(token)

			opts := tunnel.StartOptions{
				Protocol:   tunnel.ProtocolTLS,
				Region:     region,
				AuthToken:  token,
				AllowedIPs: allowedIPs,
				QREnabled:  qrCode,
				HTTPS:      cfg.Tunnel.TLSVerify,
			}

			logger.Info("Starting TLS tunnel (port %d)...", port)

			t, err := mgr.Start(ctx, port, opts)
			if err != nil {
				return fmt.Errorf("TLS tunnel failed: %w", err)
			}

			if jsonOutput {
				fmt.Printf(`{"url":"%s","id":"%s","port":%d,"protocol":"tls"}`+"\n", t.PublicURL, t.ID, port)
			} else {
				logger.PrintURL(t.PublicURL)
				printTLSInfo(t, port, opts)
			}

			<-ctx.Done()

			fmt.Println()
			logger.Info("Closing TLS tunnel %s...", t.ID)
			mgr.Remove(t.ID)

			return nil
		},
	}

	cmd.Flags().IntVarP(&port, "port", "p", 0, "Local TLS port to expose (required)")
	cmd.Flags().BoolVar(&noOpen, "no-open", false, "Don't auto-open browser")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Output in JSON format")
	cmd.Flags().BoolVar(&qrCode, "qr", false, "Display QR code for the public URL")
	cmd.Flags().StringSliceVar(&allowedIPs, "allow-ip", nil, "Not supported for raw tunnels yet (returns an error)")
	cmd.Flags().StringVar(&region, "region", "", "Relay region (e.g. ams, sea, sin)")

	_ = cmd.MarkFlagRequired("port")

	return cmd
}

func printTLSInfo(t *tunnel.Tunnel, port int, opts tunnel.StartOptions) {
	fmt.Println()
	term.Green.Printf("  => ")
	fmt.Printf("localhost:%d", port)
	term.Dim.Print("  →  ")
	term.Cyan.Println(t.PublicURL)
	term.Yellow.Println("  Protocol:   TLS over a raw tunnel (no SNI passthrough)")
	fmt.Println()
	printRawTunnelNote(t.PublicURL)
	fmt.Println()
	term.Dim.Println("  Press Ctrl+C to disconnect")
	fmt.Println()
}
