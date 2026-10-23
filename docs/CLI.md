# Tunr CLI Reference

This document is the official usage reference for the `tunr` command-line interface (CLI).

Commands are grouped by product: **Agent Bridge** first (connect AI agents to
data inside a customer's network), then tunnels, logs and maintenance.

Every command accepts the global flags `--relay <url>` (self-hosted relay;
default `https://relay.tunr.sh`, or `$TUNR_RELAY_URL`) and `-v, --verbose`.

## Agent Bridge (preview)

The bridge has three roles, each with its own commands:

| Who | Command | What it does |
|-----|---------|--------------|
| Customer IT | `tunr connect` | Runs the connector inside the customer's network (outbound only) and serves named resources |
| Customer approvers | `tunr grants` | Approve, deny and revoke agent access |
| Vendor | `tunr agent` | Manage agent identities, tokens and (early access) hosted agents |
| Vendor's agent | `tunr catalog`, `tunr access` | Find resources, request a grant, open a local port |

Walkthroughs: [customer quick start](bridge/quickstart-customer.md) ·
[vendor quick start](bridge/quickstart-vendor.md). Guarantees and limits:
[bridge/security.md](bridge/security.md). Wire protocol:
[protocol/bridge.md](protocol/bridge.md).

Agent-side commands (`tunr catalog`, `tunr access ...`) authenticate with an
**agent token**, not your login: set `TUNR_AGENT_TOKEN` (see
[`tunr agent token`](#tunr-agent)). Inside a hosted agent, `TUNR_AGENT_BOOTSTRAP`
is set automatically and exchanged for short-lived tokens. Human-side commands
(`tunr agent`, `tunr grants`) use the account from `tunr login`.

### `tunr connect`
Run the Agent Bridge site connector. It runs inside your network, only makes
outbound connections to the relay, and serves the named resources in its local
config — their addresses never leave the machine — and only for approved grants.

First run: paste the command the portal shows when you create a site. Then add
resources with `tunr connect resource add` and install it as a service with
`tunr connect install`. With no subcommand and an existing config, it runs the
connector in the foreground.

**Flags:**
* `--site <string>`: Site name (first-time setup).
* `--token-stdin`: Read the site token from a hidden prompt, or from stdin when piped (first-time setup). Stored in the secret store, not the config file.
* `--token <string>`: Site token on the command line. Prefer `--token-stdin`: a token in argv ends up in shell history. With `--token` the command only does setup and exits.
* `--relay-key <string>`: Relay public key to pin (`ed25519:…`, from the portal; first-time setup). The connector refuses a relay that presents a different key.
* `--secret-store <string>`: Where secrets live: `keychain` (default) or `file` (headless servers; `0600` files in a `0700` directory, not encrypted). `tunr connect install` on Linux requires `file`.
* `--setup-only`: Write the config and store the token, then exit.
* `--config <string>`: Connector config path (default: `connector.json` in the user config directory).
* `--health-addr <string>`: Serve `GET /healthz` on this address (e.g. `127.0.0.1:9095`); `200` when connected, `503` otherwise.

```bash
# First-time setup (values come from the portal)
sudo tunr connect --relay https://bridge.tunr.sh --site istanbul-office \
  --relay-key ed25519:… --token-stdin --secret-store file --setup-only
# paste the site token (tunr_site_…) at the prompt

# Run in the foreground with the saved config
tunr connect
```

#### `tunr connect resource add NAME`
Add or update a resource. The name becomes the second half of the resource
address (`<customer-org>/<name>`).

* `--kind <string>` (required): `postgres` or `tcp`.
* `--target <host:port>` (required): Address inside your network. Stays in the local config.
* `--database <string>`: Database name (`postgres`).
* `--user <string>`: Read-only database user. The password is prompted and kept in the secret store.
* `--password-stdin`: Read the password from stdin.

Postgres resources are always served read-only through the connector's
Postgres proxy. Raw `tcp` resources cannot be made read-only, so they only open
for `rw` grants, which need two different approvers.

```bash
tunr connect resource add erp-db --kind postgres --target 10.0.0.5:5432 --database logo --user tunr_ro
tunr connect resource add nas --kind tcp --target 10.0.0.9:445
```

#### `tunr connect resource list` · `tunr connect resource remove NAME`
List resources, or remove one together with its stored credential.

#### `tunr connect pg-role`
Print SQL (for `psql`) that creates a login role which can only read the listed
tables. Give it to your DBA. A fresh password is generated each time. With
`--resource`, the role and password are also stored as that resource's
credential, so nobody copies the password by hand.

* `--database <string>` (required): Database.
* `--tables <string>`: Comma-separated tables (`schema.table` or `table`); default: all tables in the schemas.
* `--schemas <string>`: Comma-separated schemas (default `public`).
* `--role <string>`: Role name (default `tunr_agent_ro`).
* `--resource <string>`: Store the credential for this postgres resource.

```bash
tunr connect pg-role --database logo --tables fatura,stok --resource erp-db > pg-role.sql
psql -h 10.0.0.5 -U postgres -f pg-role.sql
```

#### `tunr connect install` · `tunr connect uninstall`
Install the connector as a service (systemd on Linux, launchd on macOS), or
remove it. Uninstall keeps the config and secrets.

* `--user <string>` (install, Linux): Run the service as this user (default `root`).

#### `tunr connect status`
Show the connector configuration, kill-switch state and the site's end-to-end
TLS certificate fingerprint (give it to vendors so they can check it out of band).

* `--json`: JSON output.

#### `tunr connect kill` · `tunr connect resume`
`kill` engages the local kill switch: a running connector closes all open
streams and disconnects within about a second, and stays disconnected — even
after a restart or reboot — until `tunr connect resume`. It works without the
relay.

### `tunr grants`
Review and decide agent access to your resources (customer side). All
subcommands take `--org <customer-org-slug>`.

* `tunr grants list [--status <status>]`: List grants on your resources, pending first.
* `tunr grants approve GRANT_ID`: Approve a grant. `rw` grants need a second, different approver.
* `tunr grants deny GRANT_ID`: Deny a pending grant.
* `tunr grants revoke GRANT_ID`: Revoke an approved grant and cut its open streams.

### `tunr agent`
Manage your organization's agent identities (vendor side). All subcommands take
`--org <vendor-org-slug>`.

* `tunr agent create NAME`: Create an agent identity. Customers see its name.
* `tunr agent list`: List agents.
* `tunr agent token NAME [--ttl <duration>]`: Issue a token for the agent, printed once to stdout. `--ttl` is 1m–24h (default `24h`).

```bash
tunr agent create invoice-reconciler --org vendor-co
export TUNR_AGENT_TOKEN=$(tunr agent token invoice-reconciler --org vendor-co --ttl 8h)
```

#### Hosted agents (early access)
`tunr agent deploy` and `tunr agent trigger` run an agent on tunr instead of
your own infrastructure. They are **off on tunr.sh** until egress and CPU quotas
are enforced; a self-hosted relay enables them with
`TUNR_BRIDGE_HOSTED_AGENTS=1` (see [SELF_HOSTING.md](SELF_HOSTING.md)). When
they are off, the relay answers `503 unavailable`.

`tunr agent deploy AGENT [dir]` builds the directory (like `tunr deploy`) and
runs it as `AGENT`. The agent gets its identity automatically
(`TUNR_AGENT_BOOTSTRAP`, used by the bundled `tunr` CLI and MCP tools), and is
private: it only receives signed triggers. It must listen on `$PORT` and handle
`POST` on its trigger path (default `/run`).

* `--app <string>`: App name (default: the agent name).
* `--port <int>`: Port the agent listens on (default `8080`).
* `--env KEY=VALUE`: Environment variable (repeatable; `TUNR_*` names are reserved).
* `--volume <int>`: Persistent `/data` volume in MB (`0` = none).

`tunr agent trigger` manages what wakes a hosted agent:

* `tunr agent trigger add AGENT --cron "<5-field cron, UTC>" [--name <name>]`: Add a cron trigger. Adding the same `--name` again (default `schedule`) replaces its schedule.
* `tunr agent trigger add AGENT --webhook <name>`: Add a webhook trigger; the name becomes a URL segment.
* `--path <string>`: Path the agent receives the trigger on (default `/run`).
* `tunr agent trigger list AGENT`: List triggers.
* `tunr agent trigger remove TRIGGER_ID`: Remove a trigger.

```bash
tunr agent deploy invoice-reconciler --org finbot --volume 256 ./agent
tunr agent trigger add invoice-reconciler --org finbot --cron "0 7 * * *"
tunr agent trigger add invoice-reconciler --org finbot --webhook new-invoice --path /hooks/invoice
```

### `tunr catalog`
List the customer resources your agent can request, with the current grant
status for each. Uses `TUNR_AGENT_TOKEN`.

### `tunr access`
Open a port on `127.0.0.1` that reaches a customer's resource through the
bridge. Needs an agent token and an approved grant. The resource's real address
never reaches this machine. For a `postgres` resource it prints a read-only
`postgresql://` DSN (alone on stdout, so scripts can capture it); the DSN's
password is a random per-process secret that only unlocks this local port.

```bash
tunr access CUSTOMER/RESOURCE [flags]
```

* `--port <int>`: Local port on `127.0.0.1` (default: a free port).
* `--grant <string>`: Use a specific grant ID.
* `--token-stdin`: Read the agent token from stdin instead of `TUNR_AGENT_TOKEN`.

Streams are end-to-end TLS 1.3 to the customer's connector when the connector
supports it, pinned to the site certificate fingerprint the relay reports. The
fingerprint is logged when a stream opens, so you can compare it with the one
the customer gives you.

```bash
export TUNR_AGENT_TOKEN=…
tunr access acme/nas --port 14455
tunr access acme/erp-db --port 15432   # prints a read-only postgresql:// DSN
```

#### `tunr access request CUSTOMER/RESOURCE`
Ask the customer for access. The customer's approvers read the reason.

* `--reason <string>` (required): Why you need it (up to 500 characters).
* `--ttl <string>`: How long access is needed (default `1h`). The customer org sets the maximum (8h unless changed).
* `--mode <string>`: `ro` or `rw` (default: `ro` for postgres, `rw` for tcp).
* `--wait <duration>`: Wait up to this long for a decision.

```bash
tunr access request acme/erp-db --ttl 2h --reason "October invoice reconciliation" --wait 10m
```

#### `tunr access list` · `tunr access status GRANT_ID`
List your agent's grants (`--status pending|approved|denied|revoked|expired`),
or show one grant.

---

## Setup & Health Check

### `tunr doctor`
Verify that your system and tunr installation are healthy and ready to go.
* Binary and connectivity checks
* Configuration file validation
* Relay server reachability
* OS-level service/daemon status

### `tunr config`
Manage workspace and user settings.
* `tunr config init`: Creates a `.tunr.json` configuration file in the current project directory.

---

## Tunnel Management (Core Commands)

### `tunr share`
Expose a local port to the public internet via a secure tunnel URL in under 3 seconds.

**Required Flags:**
* `-p, --port <int>`: The local port to forward (e.g. `3000`).

**Optional Flags:**
* `-s, --subdomain <string>`: Reserve a custom subdomain (Pro accounts only). Example: `myapp` → `myapp.tunr.sh`
* `--domain <string>`: Use a fully custom domain instead of the default `*.tunr.sh` subdomain. Point your domain's DNS to Tunr's relay and pass it here. Example: `--domain demo.mycompany.com`
* `--no-open`: Prevent the browser from opening automatically when the tunnel starts.
* `--json`: Output tunnel information as JSON instead of the human-friendly table. Useful for CI/CD pipelines and MCP agent integrations.

**Vibecoder Demo Flags:**
Advanced pro-proxy flags designed for freelancers and agencies to deliver flawless product demos to clients and prevent live-demo disasters.

* `--demo`: Enables read-only mode. Intercepts and blocks destructive `POST`, `PUT`, `PATCH`, and `DELETE` requests at the proxy layer. Returns a fake success (`201` for `POST`, `200` otherwise) that echoes back a JSON request body, or `{"status": "demo_success"}` for other bodies. GraphQL queries sent over `POST` still go through, and messages the visitor sends over WebSockets are dropped (HMR sockets are exempt). The client can click every button, but "Delete Order" will never touch your database.
* `--demo-allow <rule>` / `--demo-block <rule>`: Repeatable rules that override the demo defaults, written as `[METHOD ]/path[*]`. Example: `--demo-allow "POST /api/search" --demo-block "GET /logout"`. `WS /path` targets messages sent over a WebSocket. Block rules win.
* `--freeze`: Crash-Tolerance Mode. If your local server crashes or returns errors, the proxy continues serving the last successful `2xx` responses (HTML, CSS, images, JSON) from its in-memory cache (`X-Tunr-Freeze-Cache`). The client never sees a thing.
* `--inject-widget`: Adds a 💬 Feedback button and a JavaScript error catcher to every served HTML document. A client's note arrives in your terminal with the page URL and screen size; uncaught errors and unhandled promise rejections from their browser are logged there too.
* `--auto-login <string>`: Automatically injects an auth cookie or JWT `Authorization` header into every incoming request. Example: `--auto-login "Cookie: session=demo-user-1"`. When the client opens your link, they land directly in the authenticated dashboard — no login screen.

#### Examples
```bash
# Simple share
tunr share --port 8080

# With a reserved subdomain
tunr share --port 8080 -s myapp

# With a custom domain
tunr share --port 8080 --domain demo.mycompany.com

# Full Vibecoder client demo package
tunr share -p 3000 --demo --freeze --inject-widget --auto-login "Cookie: session=xyz"

# JSON output for CI/CD
tunr share --port 3000 --json

# Multi-region routing
tunr share --port 3000 --region ams    # Europe (Amsterdam)
tunr share --port 3000 --region sea    # US West (Seattle)
tunr share --port 3000 --region sin    # Asia (Singapore)

# Security features
tunr share --port 3000 --qr                    # QR code for mobile sharing
tunr share --port 3000 --auth-token "secret"   # Bearer token
tunr share --port 3000 --allow-ip 10.0.0.0/8   # IP whitelist

# Header modification
tunr share --port 3000 --header-add "X-Debug: 1"
tunr share --port 3000 --header-replace "Host: new-host"
tunr share --port 3000 --header-remove "X-Powered-By"

# Forwarded headers
tunr share --port 3000 --x-forwarded-for --original-url

# CORS preflight
tunr share --port 3000 --cors-origin "https://myapp.com"
```

---

## Background & Daemon Mode

### `tunr start`
Open a persistent tunnel that runs in the background as a daemon. The tunnel stays alive even if you close the terminal or the server restarts.
```bash
tunr start --port 3000
```

### `tunr stop`
Gracefully shut down all background tunnels.
```bash
tunr stop
```

### `tunr status`
List all active tunnels (both foreground and background) with connection stats, uptime, and URL information in a tabular format.
```bash
tunr status
```

---

## Logs & Debugging

### `tunr logs`
Display all HTTP request data flowing through your tunnels in real time via a WebSocket stream in the terminal.
```bash
tunr logs
```

**Optional Flags:**
* `--follow`: Stream logs continuously in real time, similar to `tail -f`. The command stays open and prints new entries as they arrive.
* `--flush`: Clear all stored log entries. Useful for cleaning up between debugging sessions.
* `--json`: Output log entries in JSON format for programmatic consumption or piping into other tools.

#### Examples
```bash
# Stream logs in real time
tunr logs --follow

# Clear all stored logs
tunr logs --flush

# Output logs as JSON (great for jq piping)
tunr logs --json
```

### `tunr replay`
Replay a previously recorded HTTP request by its ID, re-sending the exact same request to your local server as if the original client had triggered it again. Invaluable for reproducing and debugging issues.
```bash
tunr replay abc-123-xyz
```

### `tunr open`
Launch Tunr's embedded React dashboard (localhost Web UI) in your default system browser. Provides a visual interface for inspecting request logs, managing tunnels, and adjusting settings — ideal if you prefer a GUI over the terminal.

---

## Maintenance

### `tunr update`
Self-update the tunr CLI binary to the latest release from GitHub. Checks for a newer version and replaces the current binary in place.
```bash
tunr update
```

### `tunr uninstall`
Completely remove the tunr binary and its configuration files from the system. Stops any running daemons before cleanup.
```bash
tunr uninstall
```

---

## AI Agent Integration

### `tunr mcp`
Start a Model Context Protocol (MCP) server directly inside Claude Desktop or Cursor IDE. This allows AI agents to programmatically create tunnels, read request logs, and interact with your development environment.
```bash
tunr mcp
```

**Agent Bridge tools.** The same server exposes five bridge tools. They use the
agent token in the MCP server's environment (`TUNR_AGENT_TOKEN`, or
`TUNR_AGENT_BOOTSTRAP` inside a hosted agent), not your login; without one they
return an error explaining how to create a token.

| Tool | Parameters | What it does |
|------|------------|--------------|
| `tunr_list_resources` | — | Customer resources this agent may request, with current grant status |
| `tunr_request_access` | `resource` (required), `reason` (required, ≤500 chars), `ttl` (default `1h`), `mode` (leave empty for the default) | Ask the customer for time-limited access; returns `pending` until a person decides |
| `tunr_access_status` | `grant_id` (required) | Whether a grant is approved, denied, revoked or expired |
| `tunr_open_access` | `resource` (required), `local_port` (1024–65535, default any free port) | Open a local port to an approved resource; returns the address and, for postgres, a DSN |
| `tunr_close_access` | `resource` (required) | Close the local port opened by `tunr_open_access` |

```json
{"mcpServers": {"tunr": {"command": "tunr", "args": ["mcp"],
  "env": {"TUNR_AGENT_TOKEN": "…", "TUNR_RELAY_URL": "https://<relay>"}}}}
```

---

## Raw Tunnels (experimental)

### `tunr tcp` · `tunr udp` · `tunr tls`
Forward raw bytes from a local port through the relay.

**Read this first.** The relay exposes a raw tunnel only as a WebSocket stream
at `wss://<subdomain>.tunr.sh/tunnel/tcp?subdomain=<subdomain>` — binary frames
carry the raw bytes. There is no raw TCP/UDP port on the relay yet, so native
clients (`psql`, `ssh`, a game client) can't connect directly; you need a client
that speaks that WebSocket. Opening the public URL in a browser shows an
informational page with the endpoint.

* `tunr udp` carries datagrams over the same stream.
* `tunr tls` forwards a local TLS port over the same transport. It is **not**
  SNI passthrough: the relay terminates TLS for `*.tunr.sh`, so a normal TLS
  client reaches the relay, not your server.

**Required Flags:**
* `-p, --port <int>`: The local port to forward (e.g. `5432`).

**Optional Flags:**
* `--region <string>`: Relay region hint (see below).
* `--json`: Output tunnel information as JSON.
* `--allow-ip`: not supported on raw tunnels yet — the relay can't enforce it,
  so the command exits with an error instead of leaving the port open.

```
WebSocket client ──wss──> [tunr relay] ──WebSocket──> [tunr CLI] ──TCP/UDP──> localhost:PORT
```

---

## Multi-Region Routing

`tunr share` and the raw tunnel commands accept the `--region` flag for multi-region relay selection.

**Available Regions:**

| Code | Location | Region |
|------|----------|--------|
| `ams` | Amsterdam | Europe (EU) |
| `sea` | Seattle | US West (Americas) |
| `sin` | Singapore | Asia-Pacific |

**How it works:**
1. CLI sends the region preference during the WebSocket handshake
2. Relay stores the region preference in the tunnel registry
3. Future infrastructure will route to the appropriate edge server
4. Currently uses the single relay server (region is stored for future use)

```bash
# HTTP tunnel to European clients
tunr share --port 3000 --region ams

# TCP tunnel for Asia-Pacific users
tunr tcp --port 5432 --region sin
```
