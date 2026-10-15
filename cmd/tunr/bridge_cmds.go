package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
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

// agentTokens: TUNR_AGENT_TOKEN, or — inside a hosted agent — a source that
// exchanges TUNR_AGENT_BOOTSTRAP for hourly tokens.
func agentTokens() (*api.TokenSource, error) {
	ts := &api.TokenSource{Relay: relayURL(), Static: os.Getenv("TUNR_AGENT_TOKEN"), Bootstrap: os.Getenv("TUNR_AGENT_BOOTSTRAP")}
	if ts.Static == "" && ts.Bootstrap == "" {
		return nil, errors.New("no agent token: set TUNR_AGENT_TOKEN (create one with `tunr agent token`)")
	}
	return ts, nil
}

func humanAPI() (*api.Client, error) {
	t, err := humanToken()
	if err != nil {
		return nil, err
	}
	return &api.Client{Relay: relayURL(), Token: t}, nil
}

func agentAPI() (*api.Client, error) {
	ts, err := agentTokens()
	if err != nil {
		return nil, err
	}
	return &api.Client{Relay: relayURL(), Tokens: ts}, nil
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

	cmd.AddCommand(create, list, token, newAgentDeployCmd(&org), newAgentTriggerCmd(&org))
	return cmd
}

// tunr agent deploy — run an agent on tunr.
func newAgentDeployCmd(org *string) *cobra.Command {
	var app string
	var port, volume int
	var envs []string
	cmd := &cobra.Command{
		Use:   "deploy AGENT [dir]",
		Short: "Run an agent on tunr (builds the directory like tunr deploy)",
		Long: `Build the directory on tunr and run it as AGENT. The agent gets its identity
automatically (TUNR_AGENT_BOOTSTRAP — the bundled tunr CLI and MCP tools use
it), a /data volume with --volume, and is private: it only receives signed
triggers (tunr agent trigger add). It must listen on $PORT and handle POST
on its trigger path (default /run).`,
		Example: `  tunr agent deploy invoice-reconciler --org finbot --volume 256
  tunr agent trigger add invoice-reconciler --org finbot --cron "0 7 * * *"`,
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := requireOrg(*org); err != nil {
				return err
			}
			c, err := humanAPI()
			if err != nil {
				return err
			}
			id, err := agentIDByName(cmd.Context(), c, *org, args[0])
			if err != nil {
				return err
			}
			dir := "."
			if len(args) == 2 {
				dir = args[1]
			}
			absDir, err := filepath.Abs(dir)
			if err != nil {
				return err
			}
			envMap := map[string]string{}
			for _, e := range envs {
				if k, v, ok := strings.Cut(e, "="); ok {
					envMap[strings.TrimSpace(k)] = v
				}
			}
			if app == "" {
				app = args[0]
			}
			metaJSON, _ := json.Marshal(map[string]any{"name": app, "port": port, "env": envMap, "volume_mb": volume})
			fmt.Printf("  ▲ Packing %s…\n", absDir)
			var tarBuf bytes.Buffer
			count, err := tarDir(absDir, &tarBuf)
			if err != nil {
				return fmt.Errorf("packing failed: %w", err)
			}
			if tarBuf.Len() > 50<<20 {
				return fmt.Errorf("upload is %s (>50MB)", human(tarBuf.Len()))
			}
			fmt.Printf("  ▲ Uploading %d files, %s\n", count, human(tarBuf.Len()))
			var body bytes.Buffer
			mw := multipart.NewWriter(&body)
			_ = mw.WriteField("meta", string(metaJSON))
			fw, _ := mw.CreateFormFile("source", "source.tar.gz")
			if _, err := io.Copy(fw, &tarBuf); err != nil {
				return err
			}
			_ = mw.Close()
			req, err := http.NewRequestWithContext(cmd.Context(), http.MethodPost,
				relayURL()+"/api/v1/bridge/orgs/"+*org+"/agents/"+id+"/deploy", &body)
			if err != nil {
				return err
			}
			req.Header.Set("Authorization", "Bearer "+c.Token)
			req.Header.Set("Content-Type", mw.FormDataContentType())
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				return err
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				b, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
				return fmt.Errorf("deploy failed (%d): %s", resp.StatusCode, strings.TrimSpace(string(b)))
			}
			return streamDeploy(resp.Body)
		},
	}
	cmd.Flags().StringVar(&app, "app", "", "App name (default: the agent name)")
	cmd.Flags().IntVar(&port, "port", 0, "Port the agent listens on (default 8080)")
	cmd.Flags().IntVar(&volume, "volume", 0, "Persistent /data volume in MB (0 = none)")
	cmd.Flags().StringArrayVar(&envs, "env", nil, "Environment variable KEY=VALUE (repeatable; TUNR_* are reserved)")
	return cmd
}

// tunr agent trigger add|list|remove
func newAgentTriggerCmd(org *string) *cobra.Command {
	cmd := &cobra.Command{Use: "trigger", Short: "Cron and webhook triggers for a hosted agent"}
	var cron, webhook, path, name string
	add := &cobra.Command{
		Use: "add AGENT", Short: "Add a cron (UTC) or webhook trigger", Args: cobra.ExactArgs(1),
		Example: `  tunr agent trigger add invoice-reconciler --org finbot --cron "0 7 * * *"
  tunr agent trigger add invoice-reconciler --org finbot --webhook new-invoice --path /hooks/invoice`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := requireOrg(*org); err != nil {
				return err
			}
			if (cron == "") == (webhook == "") {
				return errors.New("give exactly one of --cron or --webhook")
			}
			c, err := humanAPI()
			if err != nil {
				return err
			}
			id, err := agentIDByName(cmd.Context(), c, *org, args[0])
			if err != nil {
				return err
			}
			in := map[string]string{"path": path}
			if cron != "" {
				in["kind"], in["spec"], in["name"] = "cron", cron, name
			} else {
				in["kind"], in["name"] = "webhook", webhook
			}
			var out struct {
				Trigger                struct{ ID, Kind, Name, Spec, Path string }
				URL, Secret, Signature string
			}
			if err := c.Do(cmd.Context(), http.MethodPost, "/orgs/"+*org+"/agents/"+id+"/triggers", in, &out); err != nil {
				return err
			}
			if out.Trigger.Kind == "cron" {
				term.Green.Println(fmt.Sprintf("  ✓ cron %q (UTC) → POST %s", out.Trigger.Spec, out.Trigger.Path))
				return nil
			}
			term.Green.Println(fmt.Sprintf("  ✓ webhook %s → POST %s", out.Trigger.Name, out.Trigger.Path))
			fmt.Println("  URL:    " + out.URL)
			fmt.Println("  Secret: " + out.Secret + "   (shown once)")
			term.Dim.Println("  Callers sign: " + out.Signature)
			return nil
		},
	}
	add.Flags().StringVar(&cron, "cron", "", `5-field cron in UTC, e.g. "0 7 * * *"`)
	add.Flags().StringVar(&webhook, "webhook", "", "Webhook name (URL segment)")
	add.Flags().StringVar(&path, "path", "/run", "Path the agent receives the trigger on")
	add.Flags().StringVar(&name, "name", "schedule", "Cron trigger name (adding the same name again replaces its schedule)")

	list := &cobra.Command{
		Use: "list AGENT", Short: "List triggers", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := requireOrg(*org); err != nil {
				return err
			}
			c, err := humanAPI()
			if err != nil {
				return err
			}
			id, err := agentIDByName(cmd.Context(), c, *org, args[0])
			if err != nil {
				return err
			}
			var out struct {
				Triggers []struct {
					ID, Kind, Name, Spec, Path string
					LastFired                  *time.Time `json:"last_fired"`
				}
			}
			if err := c.Do(cmd.Context(), http.MethodGet, "/orgs/"+*org+"/agents/"+id+"/triggers", nil, &out); err != nil {
				return err
			}
			tw := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "ID\tKIND\tNAME\tSCHEDULE\tPATH\tLAST FIRED")
			for _, t := range out.Triggers {
				last := "-"
				if t.LastFired != nil {
					last = t.LastFired.Local().Format(time.RFC822)
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", t.ID, t.Kind, t.Name, t.Spec, t.Path, last)
			}
			return tw.Flush()
		},
	}
	remove := &cobra.Command{
		Use: "remove TRIGGER_ID", Short: "Remove a trigger", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := requireOrg(*org); err != nil {
				return err
			}
			c, err := humanAPI()
			if err != nil {
				return err
			}
			if err := c.Do(cmd.Context(), http.MethodDelete, "/orgs/"+*org+"/triggers/"+args[0], nil, nil); err != nil {
				return err
			}
			term.Green.Println("  ✓ trigger removed")
			return nil
		},
	}
	cmd.AddCommand(add, list, remove)
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
