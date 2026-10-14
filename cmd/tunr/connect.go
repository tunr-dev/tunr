package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"text/tabwriter"

	"github.com/spf13/cobra"
	xterm "golang.org/x/term"

	"github.com/tunr-dev/tunr/internal/bridge"
	"github.com/tunr-dev/tunr/internal/bridge/connector"
	"github.com/tunr-dev/tunr/internal/logger"
	"github.com/tunr-dev/tunr/internal/term"
	"github.com/tunr-dev/tunr/internal/tunnel"
)

// connect.go — `tunr connect`: the Agent Bridge site connector (customer side).
//
//	tunr connect --relay URL --site NAME --token TOKEN --relay-key KEY   # first run (from the portal)
//	tunr connect resource add erp-db --kind postgres --target 10.0.0.5:5432 --database logo --user tunr_ro
//	tunr connect                                                          # run (foreground)
//	sudo tunr connect install                                             # run as a service
//	tunr connect kill | resume                                            # local emergency stop

var connectConfigPath string

func connectCfgPath() string {
	if connectConfigPath != "" {
		return connectConfigPath
	}
	return connector.DefaultPath()
}

func newConnectCmd() *cobra.Command {
	var (
		site, token, relayKey, secretStore string
		tokenStdin, setupOnly              bool
	)
	cmd := &cobra.Command{
		Use:   "connect",
		Short: "Run the Agent Bridge connector for a customer site",
		Long: `Run the Agent Bridge site connector.

The connector runs inside your network and only makes outbound connections to
the relay. It serves the named resources in its local config — their addresses
never leave this machine — and only for grants you approved.

First run: paste the command shown when you created the site in the portal.
Then add resources with 'tunr connect resource add', and install it as a
service with 'tunr connect install'.`,
		Example: `  tunr connect --relay https://bridge.tunr.sh --site istanbul-ofis --token tunr_site_… --relay-key ed25519:…
  tunr connect resource add erp-db --kind postgres --target 10.0.0.5:5432 --database logo --user tunr_ro
  tunr connect`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			path := connectCfgPath()
			if tokenStdin {
				t, err := readLine(os.Stdin)
				if err != nil {
					return fmt.Errorf("read token from stdin: %w", err)
				}
				token = t
			}
			if token != "" {
				if err := connectSetup(path, site, token, relayKey, secretStore); err != nil {
					return err
				}
				if setupOnly {
					return nil
				}
			} else if site != "" || relayKey != "" {
				return errors.New("--site and --relay-key are only used together with --token (first-time setup)")
			}
			return connectRun(cmd.Context(), path)
		},
	}
	cmd.PersistentFlags().StringVar(&connectConfigPath, "config", "", "Connector config path (default "+connector.DefaultPath()+")")
	cmd.Flags().StringVar(&site, "site", "", "Site name (first-time setup)")
	cmd.Flags().StringVar(&token, "token", "", "Site token from the portal (first-time setup; stored in the secret store, not the config)")
	cmd.Flags().BoolVar(&tokenStdin, "token-stdin", false, "Read the site token from stdin instead of --token")
	cmd.Flags().StringVar(&relayKey, "relay-key", "", "Relay public key to pin, from the portal (first-time setup)")
	cmd.Flags().StringVar(&secretStore, "secret-store", "", `Where secrets live: "keychain" (default) or "file" (headless servers)`)
	cmd.Flags().BoolVar(&setupOnly, "setup-only", false, "Write config and store the token, then exit")

	cmd.AddCommand(newConnectResourceCmd(), newConnectStatusCmd(), newConnectKillCmd(), newConnectResumeCmd(),
		newConnectInstallCmd(), newConnectUninstallCmd(), newConnectPGRoleCmd())
	return cmd
}

func connectSetup(path, site, token, relayKey, secretStore string) error {
	if !strings.HasPrefix(token, "tunr_site_") {
		return errors.New("that does not look like a site token (tunr_site_…)")
	}
	cfg, err := connector.Load(path)
	if err != nil {
		if _, statErr := os.Stat(path); statErr == nil {
			return err // exists but broken: don't overwrite it
		}
		cfg = &connector.Config{}
	}
	if relayFlag != "" || cfg.Relay == "" {
		cfg.Relay = relayURL()
	}
	if site != "" {
		cfg.Site = site
	}
	if relayKey != "" {
		if cfg.RelayKey != "" && cfg.RelayKey != relayKey {
			term.Yellow.Println("  ! Relay key changed from the pinned value. Only continue if the portal shows the new key.")
		}
		cfg.RelayKey = relayKey
	}
	if secretStore != "" {
		cfg.SecretStore = secretStore
	}
	if err := cfg.Validate(); err != nil {
		return err
	}
	if err := cfg.Secrets(path).Set(connector.SiteTokenSecret, token); err != nil {
		return fmt.Errorf("store site token: %w", err)
	}
	if err := cfg.Save(path); err != nil {
		return err
	}
	term.Green.Println(fmt.Sprintf("  ✓ Site %s configured (%s)", cfg.Site, path))
	if len(cfg.Resources) == 0 {
		term.Dim.Println("  Next: tunr connect resource add <name> --kind postgres|tcp --target host:port")
	}
	return nil
}

func connectRun(ctx context.Context, path string) error {
	cfg, err := connector.Load(path)
	if err != nil {
		return err
	}
	token, err := cfg.Secrets(path).Get(connector.SiteTokenSecret)
	if err != nil {
		return fmt.Errorf("site token not found in the %s secret store — run first-time setup again: %w",
			storeName(cfg), err)
	}
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	fmt.Println()
	term.Bold.Println(fmt.Sprintf("  tunr connector · site %s → %s", cfg.Site, cfg.Relay))
	for _, r := range cfg.Resources {
		term.Dim.Println(fmt.Sprintf("    %s (%s)", r.Name, r.Kind))
	}
	logf := func(f string, a ...any) { logger.Info(f, a...) }
	streams, err := connector.NewManager(cfg, logf)
	if err != nil {
		return err
	}
	cl := &connector.Client{
		Cfg: cfg, Token: token, Version: tunnel.Version, KillPath: connector.KillPath(path),
		Streams: streams, Logf: logf,
		OnReady: func(w bridge.WelcomeData) {
			term.Green.Println(fmt.Sprintf("  ● online — %d resources registered", len(w.Resources)))
		},
	}
	err = cl.Run(ctx)
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

func storeName(cfg *connector.Config) string {
	if cfg.SecretStore == "" {
		return "keychain"
	}
	return cfg.SecretStore
}

// ── resources ────────────────────────────────────────────────────────────────

func newConnectResourceCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "resource", Short: "Manage the resources this site exposes"}

	var kind, target, database, user string
	var passwordStdin bool
	add := &cobra.Command{
		Use:   "add NAME",
		Short: "Add or update a resource",
		Example: `  tunr connect resource add erp-db --kind postgres --target 10.0.0.5:5432 --database logo --user tunr_ro
  tunr connect resource add nas --kind tcp --target 10.0.0.9:445`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			path := connectCfgPath()
			cfg, err := connector.Load(path)
			if err != nil {
				return err
			}
			r := connector.Resource{Name: args[0], Kind: kind, Target: target, Database: database}
			if user != "" {
				if kind != "postgres" {
					return errors.New("--user is only for postgres resources")
				}
				pw, err := readPassword(passwordStdin, fmt.Sprintf("  Password for %s: ", user))
				if err != nil {
					return err
				}
				cred, _ := json.Marshal(map[string]string{"user": user, "password": pw})
				r.Credential = "cred-" + r.Name
				if err := cfg.Secrets(path).Set(r.Credential, string(cred)); err != nil {
					return fmt.Errorf("store credential: %w", err)
				}
			} else if old, ok := cfg.Resource(r.Name); ok {
				r.Credential = old.Credential
			}
			replaced := false
			for i := range cfg.Resources {
				if cfg.Resources[i].Name == r.Name {
					cfg.Resources[i], replaced = r, true
				}
			}
			if !replaced {
				cfg.Resources = append(cfg.Resources, r)
			}
			if err := cfg.Save(path); err != nil {
				return err
			}
			term.Green.Println(fmt.Sprintf("  ✓ %s %s (%s → %s)", map[bool]string{true: "Updated", false: "Added"}[replaced], r.Name, r.Kind, r.Target))
			term.Dim.Println("  The address stays on this machine; the relay only learns the name and kind.")
			term.Dim.Println("  Restart the connector to announce it.")
			return nil
		},
	}
	add.Flags().StringVar(&kind, "kind", "", "postgres | tcp (required)")
	add.Flags().StringVar(&target, "target", "", "host:port inside your network (required)")
	add.Flags().StringVar(&database, "database", "", "Database name (postgres)")
	add.Flags().StringVar(&user, "user", "", "Read-only database user; the password is prompted and kept in the secret store")
	add.Flags().BoolVar(&passwordStdin, "password-stdin", false, "Read the password from stdin")
	_ = add.MarkFlagRequired("kind")
	_ = add.MarkFlagRequired("target")

	remove := &cobra.Command{
		Use:   "remove NAME",
		Short: "Remove a resource (and its stored credential)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			path := connectCfgPath()
			cfg, err := connector.Load(path)
			if err != nil {
				return err
			}
			r, ok := cfg.Resource(args[0])
			if !ok {
				return fmt.Errorf("no resource %q", args[0])
			}
			if r.Credential != "" {
				_ = cfg.Secrets(path).Delete(r.Credential)
			}
			kept := cfg.Resources[:0]
			for _, x := range cfg.Resources {
				if x.Name != args[0] {
					kept = append(kept, x)
				}
			}
			cfg.Resources = kept
			if err := cfg.Save(path); err != nil {
				return err
			}
			term.Green.Println(fmt.Sprintf("  ✓ Removed %s", args[0]))
			return nil
		},
	}

	list := &cobra.Command{
		Use:   "list",
		Short: "List resources",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := connector.Load(connectCfgPath())
			if err != nil {
				return err
			}
			tw := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "NAME\tKIND\tTARGET\tDATABASE\tCREDENTIAL")
			for _, r := range cfg.Resources {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", r.Name, r.Kind, r.Target, r.Database,
					map[bool]string{true: "stored", false: "-"}[r.Credential != ""])
			}
			return tw.Flush()
		},
	}
	cmd.AddCommand(add, remove, list)
	return cmd
}

// ── status / kill / resume ───────────────────────────────────────────────────

func newConnectStatusCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show connector configuration and kill-switch state",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			path := connectCfgPath()
			cfg, err := connector.Load(path)
			if err != nil {
				return err
			}
			_, terr := cfg.Secrets(path).Get(connector.SiteTokenSecret)
			_, kerr := os.Stat(connector.KillPath(path))
			st := map[string]any{
				"config": path, "relay": cfg.Relay, "site": cfg.Site, "relay_key": cfg.RelayKey,
				"secret_store": storeName(cfg), "token_stored": terr == nil, "killed": kerr == nil,
				"resources": cfg.Decls(),
			}
			if asJSON {
				return json.NewEncoder(os.Stdout).Encode(st)
			}
			term.Bold.Println(fmt.Sprintf("  Site      %s", cfg.Site))
			fmt.Printf("  Relay     %s\n  Key       %s\n  Config    %s\n  Secrets   %s (token %s)\n",
				cfg.Relay, cfg.RelayKey, path, storeName(cfg), map[bool]string{true: "stored", false: "MISSING"}[terr == nil])
			if kerr == nil {
				term.Red.Println("  Kill switch ENGAGED — connector will not connect (tunr connect resume)")
			}
			fmt.Printf("  Resources %d\n", len(cfg.Resources))
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "JSON output")
	return cmd
}

func newConnectKillCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "kill",
		Short: "Emergency stop: close every stream and stay disconnected until 'resume'",
		Long: `Engage the local kill switch. A running connector closes all open streams
and disconnects within about a second, and will not reconnect — even after a
restart or reboot — until 'tunr connect resume'. Works without the relay.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			p := connector.KillPath(connectCfgPath())
			if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
				return err
			}
			if err := os.WriteFile(p, []byte("killed by tunr connect kill\n"), 0o600); err != nil {
				return fmt.Errorf("engage kill switch: %w", err)
			}
			term.Red.Println("  ■ Kill switch engaged — all bridge access from this site is stopped.")
			term.Dim.Println("  Undo with: tunr connect resume")
			return nil
		},
	}
}

func newConnectResumeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "resume",
		Short: "Release the kill switch",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := os.Remove(connector.KillPath(connectCfgPath())); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			term.Green.Println("  ✓ Kill switch released — the connector will reconnect.")
			return nil
		},
	}
}

// ── install / uninstall ──────────────────────────────────────────────────────

func newConnectInstallCmd() *cobra.Command {
	var user string
	cmd := &cobra.Command{
		Use:   "install",
		Short: "Install the connector as a service (systemd on Linux, launchd on macOS)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			path, _ := filepath.Abs(connectCfgPath())
			cfg, err := connector.Load(path)
			if err != nil {
				return err
			}
			if _, err := cfg.Secrets(path).Get(connector.SiteTokenSecret); err != nil {
				return errors.New("site token not stored — run first-time setup before installing")
			}
			bin, err := os.Executable()
			if err != nil {
				return err
			}
			bin, _ = filepath.Abs(bin)
			home, _ := os.UserHomeDir()
			p := connector.ServiceParams{BinaryPath: bin, ConfigPath: path, User: user, Home: home}

			switch runtime.GOOS {
			case "linux":
				// A system service has no desktop keyring session.
				if cfg.SecretStore != "file" {
					return errors.New(`on Linux the service can't reach a desktop keychain: re-run setup with --secret-store file (as root, config in /etc/tunr), then install`)
				}
				unit, err := connector.SystemdUnit(p)
				if err != nil {
					return err
				}
				if err := os.WriteFile(connector.SystemdUnitPath, []byte(unit), 0o644); err != nil {
					return fmt.Errorf("write %s (run with sudo): %w", connector.SystemdUnitPath, err)
				}
				for _, c := range [][]string{{"daemon-reload"}, {"enable", "--now", "tunr-connector"}} {
					if out, err := exec.Command("systemctl", c...).CombinedOutput(); err != nil {
						return fmt.Errorf("systemctl %s: %s", strings.Join(c, " "), strings.TrimSpace(string(out)))
					}
				}
				term.Green.Println("  ✓ tunr-connector service installed and started")
				term.Dim.Println("  Logs: sudo journalctl -u tunr-connector -f")
			case "darwin":
				plist, err := connector.LaunchdPlist(p)
				if err != nil {
					return err
				}
				dir := filepath.Join(home, "Library", "LaunchAgents")
				_ = os.MkdirAll(dir, 0o755)
				dst := filepath.Join(dir, connector.LaunchdLabel+".plist")
				if err := os.WriteFile(dst, []byte(plist), 0o644); err != nil {
					return err
				}
				_ = exec.Command("launchctl", "unload", dst).Run()
				if out, err := exec.Command("launchctl", "load", dst).CombinedOutput(); err != nil {
					return fmt.Errorf("launchctl load: %s", strings.TrimSpace(string(out)))
				}
				term.Green.Println("  ✓ tunr connector installed (runs while you are logged in)")
				term.Dim.Println("  Logs: tail -f ~/Library/Logs/tunr-connector.log")
			default:
				return fmt.Errorf("service install is not supported on %s yet", runtime.GOOS)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&user, "user", "", "Linux: run the service as this user (default root)")
	return cmd
}

func newConnectUninstallCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "uninstall",
		Short: "Remove the connector service (config and secrets are kept)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			switch runtime.GOOS {
			case "linux":
				_ = exec.Command("systemctl", "disable", "--now", "tunr-connector").Run()
				if err := os.Remove(connector.SystemdUnitPath); err != nil && !errors.Is(err, os.ErrNotExist) {
					return fmt.Errorf("remove unit (run with sudo): %w", err)
				}
				_ = exec.Command("systemctl", "daemon-reload").Run()
			case "darwin":
				home, _ := os.UserHomeDir()
				dst := filepath.Join(home, "Library", "LaunchAgents", connector.LaunchdLabel+".plist")
				_ = exec.Command("launchctl", "unload", dst).Run()
				_ = os.Remove(dst)
			default:
				return fmt.Errorf("not supported on %s", runtime.GOOS)
			}
			term.Green.Println("  ✓ Connector service removed")
			return nil
		},
	}
}

// ── pg-role ──────────────────────────────────────────────────────────────────

func newConnectPGRoleCmd() *cobra.Command {
	var role, database, schemas, tables, resource string
	cmd := &cobra.Command{
		Use:   "pg-role",
		Short: "Print SQL that creates a read-only Postgres role for the connector",
		Long: `Print the SQL (for psql) that creates a login role which can only read the
listed tables. Give it to your DBA. A fresh password is generated each time.

With --resource, the role and password are also stored as that resource's
credential, so the password never needs to be copied by hand.`,
		Example: `  tunr connect pg-role --database logo --tables fatura,stok --resource erp-db > pg-role.sql
  psql -h 10.0.0.5 -U postgres -f pg-role.sql`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			pw, err := connector.GeneratePassword()
			if err != nil {
				return err
			}
			spec := connector.PGRoleSpec{Role: role, Password: pw, Database: database,
				Schemas: splitList(schemas), Tables: splitList(tables)}
			sql, err := spec.SQL()
			if err != nil {
				return err
			}
			if resource != "" {
				path := connectCfgPath()
				cfg, err := connector.Load(path)
				if err != nil {
					return err
				}
				r, ok := cfg.Resource(resource)
				if !ok || r.Kind != "postgres" {
					return fmt.Errorf("no postgres resource %q (add it first)", resource)
				}
				cred, _ := json.Marshal(map[string]string{"user": role, "password": pw})
				r.Credential = "cred-" + r.Name
				if err := cfg.Secrets(path).Set(r.Credential, string(cred)); err != nil {
					return err
				}
				if err := cfg.Save(path); err != nil {
					return err
				}
				fmt.Fprintf(os.Stderr, "  ✓ credential for %s stored in the %s secret store\n", resource, storeName(cfg))
			}
			fmt.Print(sql)
			return nil
		},
	}
	cmd.Flags().StringVar(&role, "role", "tunr_agent_ro", "Role name")
	cmd.Flags().StringVar(&database, "database", "", "Database (required)")
	cmd.Flags().StringVar(&schemas, "schemas", "public", "Comma-separated schemas")
	cmd.Flags().StringVar(&tables, "tables", "", "Comma-separated tables (schema.table or table); default: all tables in the schemas")
	cmd.Flags().StringVar(&resource, "resource", "", "Store the credential for this postgres resource")
	_ = cmd.MarkFlagRequired("database")
	return cmd
}

// ── helpers ──────────────────────────────────────────────────────────────────

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func readLine(r io.Reader) (string, error) {
	line, err := bufio.NewReader(r).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	line = strings.TrimSpace(line)
	if line == "" {
		return "", errors.New("empty input")
	}
	return line, nil
}

func readPassword(fromStdin bool, prompt string) (string, error) {
	if fromStdin {
		return readLine(os.Stdin)
	}
	fd := int(os.Stdin.Fd())
	if !xterm.IsTerminal(fd) {
		return "", errors.New("no terminal for the password prompt; use --password-stdin")
	}
	fmt.Fprint(os.Stderr, prompt)
	b, err := xterm.ReadPassword(fd)
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return "", err
	}
	if len(b) == 0 {
		return "", errors.New("empty password")
	}
	return string(b), nil
}
