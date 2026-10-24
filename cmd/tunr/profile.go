package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/tunr-dev/tunr/internal/bridge/profile"
	"github.com/tunr-dev/tunr/internal/term"
)

// profile.go — `tunr profile`: the built-in schema profiles (Nebim V3, Logo,
// Netsis…) a customer can attach to a resource and an agent reads through
// tunr_describe_resource.

func newProfileCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "profile",
		Short: "Browse built-in schema profiles for customer databases (Nebim, Logo, Netsis)",
	}
	list := &cobra.Command{
		Use: "list", Short: "List profiles", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			tw := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "NAME\tKIND\tTABLES\tTITLE")
			for _, p := range profile.All() {
				fmt.Fprintf(tw, "%s\t%s\t%d\t%s\n", p.Name, p.Kind, len(p.Tables), p.Title)
			}
			return tw.Flush()
		},
	}
	var asJSON bool
	show := &cobra.Command{
		Use: "show NAME", Short: "Show a profile: rules, tables, example queries", Args: cobra.ExactArgs(1),
		Example: "  tunr profile show logo-tiger\n  tunr profile show nebim-v3 --json",
		RunE: func(cmd *cobra.Command, args []string) error {
			p, ok := profile.Get(args[0])
			if !ok {
				return fmt.Errorf("no profile %q (tunr profile list)", args[0])
			}
			if asJSON {
				enc := json.NewEncoder(os.Stdout)
				enc.SetIndent("", "  ")
				return enc.Encode(p)
			}
			term.Bold.Println("  " + p.Title + " (" + p.Kind + ")")
			fmt.Println("  " + p.Summary)
			fmt.Println()
			term.Bold.Println("  Rules")
			for _, r := range p.Rules {
				fmt.Println("   - " + r)
			}
			fmt.Println()
			term.Bold.Println("  Tables")
			for _, t := range p.Tables {
				fmt.Printf("   %s — %s [%s]\n", t.Name, t.Title, t.Confidence)
				cols := make([]string, 0, len(t.Columns))
				for _, c := range t.Columns {
					cols = append(cols, c[0])
				}
				term.Dim.Println("     " + strings.Join(cols, ", "))
			}
			fmt.Println()
			term.Bold.Println("  Example queries")
			for _, q := range p.Queries {
				fmt.Printf("   %s [%s]\n", q.Title, q.Confidence)
				term.Dim.Println("     " + q.SQL)
			}
			fmt.Println()
			term.Yellow.Println("  Names come from public sources and vary between versions. A customer's")
			term.Yellow.Println("  `tunr connect resource verify` checks them against the real database.")
			return nil
		},
	}
	show.Flags().BoolVar(&asJSON, "json", false, "JSON output")
	cmd.AddCommand(list, show)
	return cmd
}
