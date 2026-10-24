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
	"github.com/tunr-dev/tunr/internal/bridge/api"
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
		requireE2E bool
	)
	cmd := &cobra.Command{
		Use:   "access CUSTOMER/RESOURCE",
		Short: "Open a local port to a customer resource you have a grant for",
		Long: `Open a port on 127.0.0.1 that reaches a customer's resource through the
Agent Bridge. Needs an agent token (TUNR_AGENT_TOKEN, --token-stdin, or — in a
hosted agent — TUNR_AGENT_BOOTSTRAP, set automatically) and an
approved grant. The resource's real address never reaches this machine.`,
		Example: `  export TUNR_AGENT_TOKEN=…
  tunr access acme/nas --port 14455
  tunr access acme/erp-db --port 15432   # prints a read-only postgresql:// DSN
  tunr access acme/nebim --port 15433    # mssql: a local HTTP query endpoint (POST /query)`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ts, err := agentTokens()
			if tokenStdin {
				t, rerr := readLine(os.Stdin)
				if rerr != nil {
					return rerr
				}
				ts, err = &api.TokenSource{Relay: relayURL(), Static: t}, nil
			}
			if err != nil {
				return err
			}
			relay := relayURL()
			if err := connector.ValidateRelay(relay); err != nil {
				return err
			}
			cl := &access.Client{Relay: relay, TokenFunc: ts.Token, Resource: args[0], GrantID: grantID,
				RequireE2E: requireE2E, ClientSecret: access.NewClientSecret(), Logf: func(f string, a ...any) { logger.Info(f, a...) }}

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
			cl.SameUserOnly = chk.Kind == "tcp"
			term.Green.Println(fmt.Sprintf("  ● %s (%s, %s) → 127.0.0.1:%d", args[0], chk.Kind, chk.Mode, addr.Port))
			if chk.Kind == "tcp" && !access.PeerCheckSupported {
				term.Yellow.Println("  ! Raw TCP has no password: on this OS any local user can connect to this port while it is open.")
			}
			term.Dim.Println(fmt.Sprintf("  grant %s valid until %s", chk.GrantID, chk.GrantExpiresAt.Local().Format(time.RFC1123)))
			if chk.Kind == "postgres" {
				// The DSN goes to stdout alone so scripts can capture it.
				term.Dim.Println("  read-only; writes are refused and every query is logged for the data owner")
				fmt.Println(access.DSN(addr.String(), cl.ClientSecret))
			}
			if chk.Kind == "mssql" {
				// Query mode: the customer's connector runs each statement with
				// its read-only login and returns rows as JSON.
				term.Dim.Println("  read-only; writes are refused and every query is logged for the data owner")
				term.Dim.Println(fmt.Sprintf(`  curl -s http://%s/query -H "Authorization: Bearer $TUNR_QUERY_TOKEN" -d '{"sql":"SELECT TOP 5 name FROM sys.tables"}'`, addr))
				fmt.Printf("TUNR_QUERY_URL=http://%s/query\nTUNR_QUERY_TOKEN=%s\n", addr, cl.ClientSecret)
				return access.ServeQuery(ctx, l, cl.NewQuerySession(), cl.ClientSecret)
			}
			return cl.Serve(ctx, l)
		},
	}
	cmd.AddCommand(newAccessRequestCmd(), newAccessListCmd(), newAccessStatusCmd())
	cmd.Flags().IntVar(&port, "port", 0, "Local port on 127.0.0.1 (default: a free port)")
	cmd.Flags().StringVar(&grantID, "grant", "", "Use a specific grant ID")
	cmd.Flags().BoolVar(&tokenStdin, "token-stdin", false, "Read the agent token from stdin instead of TUNR_AGENT_TOKEN")
	cmd.Flags().BoolVar(&requireE2E, "require-e2e", false, "Refuse connectors too old to end-to-end encrypt the stream")
	return cmd
}
