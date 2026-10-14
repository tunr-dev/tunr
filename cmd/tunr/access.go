package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/tunr-dev/tunr/internal/bridge/access"
	"github.com/tunr-dev/tunr/internal/bridge/connector"
	"github.com/tunr-dev/tunr/internal/logger"
	"github.com/tunr-dev/tunr/internal/term"
)

// access.go — `tunr access <customer>/<resource>`: the agent side of the
// Agent Bridge. Opens a port on 127.0.0.1; every connection to it becomes an
// authorised stream to the customer's resource.

func newAccessCmd() *cobra.Command {
	var (
		port       int
		grantID    string
		tokenStdin bool
	)
	cmd := &cobra.Command{
		Use:   "access CUSTOMER/RESOURCE",
		Short: "Open a local port to a customer resource you have a grant for",
		Long: `Open a port on 127.0.0.1 that reaches a customer's resource through the
Agent Bridge. Needs an agent token (TUNR_AGENT_TOKEN, or --token-stdin) and an
approved grant. The resource's real address never reaches this machine.`,
		Example: `  export TUNR_AGENT_TOKEN=…
  tunr access acme/nas --port 14455
  tunr access acme/erp-db --port 15432   # postgres needs the read-only proxy (coming with P5)`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			token := os.Getenv("TUNR_AGENT_TOKEN")
			if tokenStdin {
				t, err := readLine(os.Stdin)
				if err != nil {
					return err
				}
				token = t
			}
			if token == "" {
				return errors.New("no agent token: set TUNR_AGENT_TOKEN or use --token-stdin")
			}
			relay := relayURL()
			if err := connector.ValidateRelay(relay); err != nil {
				return err
			}
			cl := &access.Client{Relay: relay, Token: token, Resource: args[0], GrantID: grantID,
				Logf: func(f string, a ...any) { logger.Info(f, a...) }}

			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
			chk, err := cl.Check(cctx)
			cancel()
			if err != nil {
				var ae *access.Error
				if errors.As(err, &ae) {
					if adv := ae.Advice(args[0]); adv != "" {
						return fmt.Errorf("%s: %s\n  → %s", ae.Code, ae.Message, adv)
					}
				}
				return err
			}

			l, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
			if err != nil {
				return fmt.Errorf("listen on 127.0.0.1:%d: %w", port, err)
			}
			addr := l.Addr().(*net.TCPAddr)
			term.Green.Println(fmt.Sprintf("  ● %s (%s, %s) → 127.0.0.1:%d", args[0], chk.Kind, chk.Mode, addr.Port))
			term.Dim.Println(fmt.Sprintf("  grant %s valid until %s", chk.GrantID, chk.GrantExpiresAt.Local().Format(time.RFC1123)))
			if chk.Kind == "postgres" {
				term.Dim.Println(fmt.Sprintf("  psql \"postgres://agent:$TUNR_AGENT_TOKEN@127.0.0.1:%d/<db>\"", addr.Port))
			}
			return cl.Serve(ctx, l)
		},
	}
	cmd.Flags().IntVar(&port, "port", 0, "Local port on 127.0.0.1 (default: a free port)")
	cmd.Flags().StringVar(&grantID, "grant", "", "Use a specific grant ID")
	cmd.Flags().BoolVar(&tokenStdin, "token-stdin", false, "Read the agent token from stdin instead of TUNR_AGENT_TOKEN")
	return cmd
}
