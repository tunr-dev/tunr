package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/tunr-dev/tunr/internal/auth"
	"github.com/tunr-dev/tunr/internal/bridge/api"
	"github.com/tunr-dev/tunr/internal/term"
)

// bridge_cmds.go — Agent Bridge management from the terminal (P4-T5).
//
//	vendor  (login):  tunr agent create|list|token
//	agent   (token):  tunr catalog · tunr access request|list|status
//	customer(login):  tunr grants list|approve|deny|revoke

// humanToken: TUNR_TOKEN (CI, servers) or the keychain login.
func humanToken() (string, error) {
	if t := os.Getenv("TUNR_TOKEN"); t != "" {
		return t, nil
	}
	t, err := auth.GetToken()
	if err != nil || t == "" {
		return "", errors.New("not logged in — run `tunr login` (or set TUNR_TOKEN)")
	}
	return t, nil
}

func agentToken() (string, error) {
	if t := os.Getenv("TUNR_AGENT_TOKEN"); t != "" {
		return t, nil
	}
	return "", errors.New("no agent token: set TUNR_AGENT_TOKEN (create one with `tunr agent token`)")
}

func humanAPI() (*api.Client, error) {
	t, err := humanToken()
	if err != nil {
		return nil, err
	}
	return &api.Client{Relay: relayURL(), Token: t}, nil
}

func agentAPI() (*api.Client, error) {
	t, err := agentToken()
	if err != nil {
		return nil, err
	}
	return &api.Client{Relay: relayURL(), Token: t}, nil
}

func requireOrg(org string) error {
	if org == "" {
		return errors.New("--org is required (your org slug)")
	}
	return nil
}

func ttlString(secs int) string { return (time.Duration(secs) * time.Second).String() }

func until(t *time.Time) string {
	if t == nil {
		return "-"
	}
	d := time.Until(*t).Round(time.Minute)
	if d <= 0 {
		return "ended"
	}
	return d.String()
}

// ── tunr agent ───────────────────────────────────────────────────────────────

func newAgentCmd() *cobra.Command {
	var org string
	cmd := &cobra.Command{Use: "agent", Short: "Manage your org's agent identities (Agent Bridge, vendor side)"}
	cmd.PersistentFlags().StringVar(&org, "org", "", "Vendor org slug")

	create := &cobra.Command{
		Use: "create NAME", Short: "Create an agent identity", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := requireOrg(org); err != nil {
				return err
			}
			c, err := humanAPI()
			if err != nil {
				return err
			}
			var a struct{ ID, Name string }
			if err := c.Do(cmd.Context(), http.MethodPost, "/orgs/"+org+"/agents", map[string]string{"name": args[0]}, &a); err != nil {
				return err
			}
			term.Green.Println(fmt.Sprintf("  ✓ agent %s created (%s)", a.Name, a.ID))
			term.Dim.Println(fmt.Sprintf("  Next: tunr agent token %s --org %s", a.Name, org))
			return nil
		},
	}

	list := &cobra.Command{
		Use: "list", Short: "List agents", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := requireOrg(org); err != nil {
				return err
			}
			c, err := humanAPI()
			if err != nil {
				return err
			}
			var out struct {
				Agents []struct{ ID, Name, Kind, Status string }
			}
			if err := c.Do(cmd.Context(), http.MethodGet, "/orgs/"+org+"/agents", nil, &out); err != nil {
				return err
			}
			tw := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "NAME\tID\tKIND\tSTATUS")
			for _, a := range out.Agents {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", a.Name, a.ID, a.Kind, a.Status)
			}
			return tw.Flush()
		},
	}

	var ttl string
	token := &cobra.Command{
		Use: "token NAME", Short: "Issue a token for an agent (printed once, to stdout)", Args: cobra.ExactArgs(1),
		Example: `  export TUNR_AGENT_TOKEN=$(tunr agent token invoice-reconciler --org vendor-co --ttl 8h)`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := requireOrg(org); err != nil {
				return err
			}
			c, err := humanAPI()
			if err != nil {
				return err
			}
			id, err := agentIDByName(cmd.Context(), c, org, args[0])
			if err != nil {
				return err
			}
			var out struct {
				Token     string
				TokenID   string    `json:"token_id"`
				ExpiresAt time.Time `json:"expires_at"`
			}
			if err := c.Do(cmd.Context(), http.MethodPost, "/orgs/"+org+"/agents/"+id+"/tokens",
				map[string]string{"ttl": ttl}, &out); err != nil {
				return err
			}
			fmt.Fprintf(os.Stderr, "  ✓ token %s for %s, valid until %s (shown once)\n", out.TokenID, args[0], out.ExpiresAt.Local().Format(time.RFC1123))
			fmt.Println(out.Token)
			return nil
		},
	}
	token.Flags().StringVar(&ttl, "ttl", "24h", "Token lifetime (1m–24h)")

	cmd.AddCommand(create, list, token)
	return cmd
}

func agentIDByName(ctx context.Context, c *api.Client, org, name string) (string, error) {
	if strings.HasPrefix(name, "agt_") {
		return name, nil
	}
	var out struct{ Agents []struct{ ID, Name string } }
	if err := c.Do(ctx, http.MethodGet, "/orgs/"+org+"/agents", nil, &out); err != nil {
		return "", err
	}
	for _, a := range out.Agents {
		if a.Name == name {
			return a.ID, nil
		}
	}
	return "", fmt.Errorf("no agent %q in %s", name, org)
}

// ── tunr catalog (agent) ─────────────────────────────────────────────────────

func newCatalogCmd() *cobra.Command {
	return &cobra.Command{
		Use: "catalog", Short: "List customer resources your agent can request (uses TUNR_AGENT_TOKEN)", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := agentAPI()
			if err != nil {
				return err
			}
			items, err := c.Catalog(cmd.Context())
			if err != nil {
				return err
			}
			tw := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "RESOURCE\tKIND\tGRANT\tEXPIRES IN\tDESCRIPTION")
			for _, it := range items {
				g := it.GrantStatus
				if g == "" {
					g = "-"
				} else {
					g += " (" + it.GrantMode + ")"
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", it.Address, it.Kind, g, until(it.GrantExpiresAt), it.Description)
			}
			return tw.Flush()
		},
	}
}

// ── tunr access request|list|status (agent) ──────────────────────────────────

func newAccessRequestCmd() *cobra.Command {
	var mode, ttl, reason string
	var wait time.Duration
	cmd := &cobra.Command{
		Use: "request CUSTOMER/RESOURCE", Short: "Ask the customer for access (uses TUNR_AGENT_TOKEN)", Args: cobra.ExactArgs(1),
		Example: `  tunr access request acme/erp-db --ttl 2h --reason "October invoice reconciliation" --wait 10m`,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := agentAPI()
			if err != nil {
				return err
			}
			g, err := c.RequestGrant(cmd.Context(), api.GrantRequest{Resource: args[0], Mode: mode, TTL: ttl, Reason: reason})
			if err != nil {
				return err
			}
			printGrant(g)
			if g.Status == "pending" && wait > 0 {
				g, err = waitGrant(cmd.Context(), c, g.ID, wait)
				if err != nil {
					return err
				}
				printGrant(g)
			}
			if g.Status == "approved" {
				term.Dim.Println(fmt.Sprintf("  Next: tunr access %s --port <port>", args[0]))
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&mode, "mode", "", "ro | rw (default: ro for postgres, rw for tcp)")
	cmd.Flags().StringVar(&ttl, "ttl", "1h", "How long access is needed")
	cmd.Flags().StringVar(&reason, "reason", "", "Why — the customer reads this (required)")
	cmd.Flags().DurationVar(&wait, "wait", 0, "Wait up to this long for a decision")
	_ = cmd.MarkFlagRequired("reason")
	return cmd
}

func waitGrant(ctx context.Context, c *api.Client, id string, wait time.Duration) (*api.Grant, error) {
	ctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	t := time.NewTicker(3 * time.Second)
	defer t.Stop()
	for {
		g, err := c.AgentGrant(ctx, id)
		if err != nil {
			return nil, err
		}
		if g.Status != "pending" {
			return g, nil
		}
		select {
		case <-ctx.Done():
			return g, nil
		case <-t.C:
		}
	}
}

func printGrant(g *api.Grant) {
	style := term.Yellow
	switch g.Status {
	case "approved":
		style = term.Green
	case "denied", "revoked", "expired":
		style = term.Red
	}
	note := ""
	if g.Status == "pending" && g.FirstApproved {
		note = " — waiting for a second approver"
	}
	style.Println(fmt.Sprintf("  %s  %s %s (%s, %s)%s", g.Status, g.ID, g.Address, g.Mode, ttlString(g.RequestedTTL), note))
	if g.ExpiresAt != nil && g.Status == "approved" {
		term.Dim.Println(fmt.Sprintf("  expires in %s", until(g.ExpiresAt)))
	}
}

func newAccessListCmd() *cobra.Command {
	var status string
	cmd := &cobra.Command{
		Use: "list", Short: "List your agent's grants (uses TUNR_AGENT_TOKEN)", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := agentAPI()
			if err != nil {
				return err
			}
			gs, err := c.AgentGrants(cmd.Context(), status)
			if err != nil {
				return err
			}
			grantTable(gs, false)
			return nil
		},
	}
	cmd.Flags().StringVar(&status, "status", "", "Filter: pending | approved | denied | revoked | expired")
	return cmd
}

func newAccessStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use: "status GRANT_ID", Short: "Show one grant (uses TUNR_AGENT_TOKEN)", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := agentAPI()
			if err != nil {
				return err
			}
			g, err := c.AgentGrant(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			printGrant(g)
			return nil
		},
	}
}

func grantTable(gs []api.Grant, withAgent bool) {
	tw := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
	hdr := "ID\tRESOURCE\tMODE\tSTATUS\tTTL\tEXPIRES IN\tREASON"
	if withAgent {
		hdr = "ID\tVENDOR/AGENT\tRESOURCE\tMODE\tSTATUS\tTTL\tEXPIRES IN\tREASON"
	}
	fmt.Fprintln(tw, hdr)
	for _, g := range gs {
		status := g.Status
		if status == "pending" && g.FirstApproved {
			status = "pending (1/2)"
		}
		reason := g.Reason
		if len(reason) > 40 {
			reason = reason[:37] + "…"
		}
		if withAgent {
			fmt.Fprintf(tw, "%s\t%s/%s\t%s\t%s\t%s\t%s\t%s\t%s\n", g.ID, g.VendorSlug, g.AgentName, g.Address, g.Mode,
				status, ttlString(g.RequestedTTL), until(g.ExpiresAt), reason)
		} else {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", g.ID, g.Address, g.Mode, status,
				ttlString(g.RequestedTTL), until(g.ExpiresAt), reason)
		}
	}
	_ = tw.Flush()
}

// ── tunr grants (customer) ───────────────────────────────────────────────────

func newGrantsCmd() *cobra.Command {
	var org, status string
	cmd := &cobra.Command{Use: "grants", Short: "Review and decide agent access to your resources (Agent Bridge, customer side)"}
	cmd.PersistentFlags().StringVar(&org, "org", "", "Customer org slug")

	list := &cobra.Command{
		Use: "list", Short: "List grants on your resources (pending first)", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := requireOrg(org); err != nil {
				return err
			}
			c, err := humanAPI()
			if err != nil {
				return err
			}
			var out struct{ Grants []api.Grant }
			path := "/orgs/" + org + "/grants?side=customer"
			if status != "" {
				path += "&status=" + status
			}
			if err := c.Do(cmd.Context(), http.MethodGet, path, nil, &out); err != nil {
				return err
			}
			grantTable(out.Grants, true)
			return nil
		},
	}
	list.Flags().StringVar(&status, "status", "", "Filter by status")

	decide := func(verb, short string) *cobra.Command {
		return &cobra.Command{
			Use: verb + " GRANT_ID", Short: short, Args: cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				if err := requireOrg(org); err != nil {
					return err
				}
				c, err := humanAPI()
				if err != nil {
					return err
				}
				var g api.Grant
				if err := c.Do(cmd.Context(), http.MethodPost, "/orgs/"+org+"/grants/"+args[0]+"/"+verb, nil, &g); err != nil {
					return err
				}
				printGrant(&g)
				return nil
			},
		}
	}
	cmd.AddCommand(list,
		decide("approve", "Approve a grant (rw needs a second, different approver)"),
		decide("deny", "Deny a pending grant"),
		decide("revoke", "Revoke an approved grant and cut its open streams"),
	)
	return cmd
}
