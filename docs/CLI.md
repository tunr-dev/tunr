# Tunr CLI Reference

This document is the official usage reference for the `tunr` command-line interface (CLI).

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

* `--demo`: Enables read-only mode. Intercepts and blocks destructive `POST`, `PUT`, `PATCH`, and `DELETE` requests at the proxy layer. Returns realistic mock JSON responses such as `{"status": "demo_success"}` with a `200 OK` status code. The client can click every button — but "Delete Order" will never touch your database.
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
