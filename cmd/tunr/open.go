package main

import (
	"fmt"

	"github.com/spf13/cobra"
	"github.com/tunr-dev/tunr/internal/logger"
)

func newOpenCmd() *cobra.Command {
	var dashPort int

	cmd := &cobra.Command{
		Use:   "open",
		Short: "Open the HTTP inspector dashboard in browser",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			url := fmt.Sprintf("http://localhost:%d", dashPort)
			logger.Info("Opening dashboard: %s", url)
			openBrowser(url)
			return nil
		},
	}

	cmd.Flags().IntVar(&dashPort, "port", 19842, "Dashboard port")
	return cmd
}
