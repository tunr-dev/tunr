package main

import (
	"fmt"
	"net/http"
	"os"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/tunr-dev/tunr/internal/term"
)

// invite.go — `tunr invite`: a vendor's one-link onboarding for a customer
// ("connect your ERP to us"). Accepting the link links the vendor and creates
// the customer's first site; it grants nothing by itself.

func newInviteCmd() *cobra.Command {
	var org string
	cmd := &cobra.Command{Use: "invite", Short: "Send a customer a link to connect their systems (Agent Bridge, vendor side)"}
	cmd.PersistentFlags().StringVar(&org, "org", "", "Vendor org slug")

	var hint, note, ttl string
	create := &cobra.Command{
		Use: "create", Short: "Create a one-time connect link", Args: cobra.NoArgs,
		Example: `  tunr invite create --org finbot --customer "Kuzucular Yapı" --note "Nebim stok verisi için"`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := requireOrg(org); err != nil {
				return err
			}
			c, err := humanAPI()
			if err != nil {
				return err
			}
			var out struct {
				URL    string
				Invite struct {
					ID        string    `json:"id"`
					ExpiresAt time.Time `json:"expires_at"`
				}
			}
			body := map[string]string{"customer_hint": hint, "note": note}
			if ttl != "" {
				body["ttl"] = ttl
			}
			if err := c.Do(cmd.Context(), http.MethodPost, "/orgs/"+org+"/invites", body, &out); err != nil {
				return err
			}
			term.Green.Println("  ✓ Connect link (one use, until " + out.Invite.ExpiresAt.Local().Format("2 Jan 15:04") + "):")
			fmt.Println(out.URL)
			term.Dim.Println("  Send it to the customer's IT. Accepting links your org and creates their first site;")
			term.Dim.Println("  every grant is still approved by them, one by one.")
			return nil
		},
	}
	create.Flags().StringVar(&hint, "customer", "", "Who it is for (shown to you in the list, and to them)")
	create.Flags().StringVar(&note, "note", "", "Short message shown on the connect page")
	create.Flags().StringVar(&ttl, "ttl", "", "How long the link works (1h–720h, default 168h)")

	list := &cobra.Command{
		Use: "list", Short: "List connect links", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := requireOrg(org); err != nil {
				return err
			}
			c, err := humanAPI()
			if err != nil {
				return err
			}
			var out struct {
				Invites []struct {
					ID           string    `json:"id"`
					CustomerHint string    `json:"customer_hint"`
					CustomerSlug string    `json:"customer_slug"`
					Status       string    `json:"status"`
					ExpiresAt    time.Time `json:"expires_at"`
				}
			}
			if err := c.Do(cmd.Context(), http.MethodGet, "/orgs/"+org+"/invites", nil, &out); err != nil {
				return err
			}
			tw := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "ID\tFOR\tSTATUS\tCUSTOMER ORG\tEXPIRES")
			for _, i := range out.Invites {
				cust := i.CustomerSlug
				if cust == "" {
					cust = "-"
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", i.ID, i.CustomerHint, i.Status, cust, i.ExpiresAt.Local().Format("2006-01-02 15:04"))
			}
			return tw.Flush()
		},
	}

	revoke := &cobra.Command{
		Use: "revoke ID", Short: "Revoke a pending link", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := requireOrg(org); err != nil {
				return err
			}
			c, err := humanAPI()
			if err != nil {
				return err
			}
			if err := c.Do(cmd.Context(), http.MethodDelete, "/orgs/"+org+"/invites/"+args[0], nil, nil); err != nil {
				return err
			}
			term.Green.Println("  ✓ revoked " + args[0])
			return nil
		},
	}
	cmd.AddCommand(create, list, revoke)
	return cmd
}
