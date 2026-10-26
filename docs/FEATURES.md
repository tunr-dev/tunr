# Tunr — Core Features

Tunr's main product is the **Agent Bridge**: it connects AI agents to data inside a customer's network — read-only, time-boxed, approved by the customer and audited — through one outbound-only connector. Tunnels (`tunr share`) and cloud deploy (`tunr deploy`, preview) remain as secondary products; tunnel features are listed further down.

# Agent Bridge (preview)

Status: preview. PostgreSQL, SQL Server (query mode) and raw TCP resources today. Command reference: [CLI.md](CLI.md#agent-bridge-preview). Guarantees and limits: [bridge/security.md](bridge/security.md).

## 🔌 Connector (customer side)
- **Outbound only:** `tunr connect` runs inside the customer's network and dials the relay over HTTPS/WebSocket. No inbound port is opened.
- **Named resources:** Each resource has an address like `acme/erp-db`. The target address stays in the connector's local config; the relay and the vendor never learn it.
- **Runs as a service:** `tunr connect install` (systemd on Linux, launchd on macOS), with a `--health-addr` endpoint for monitoring.
- **Relay key pinned at install:** The connector verifies every stream's grant token against the relay key from the install command.
- **Local kill switch:** `tunr connect kill` closes every stream and keeps the connector disconnected until `tunr connect resume`, even without the relay.

## ✅ Grants & approvals
- **Default deny:** Nothing flows without an approved, unexpired grant for this agent, this resource, this mode.
- **Time-boxed:** Grants run from 1 minute up to the customer org's maximum (8h by default) and expire on their own.
- **Reason required:** The agent states why it needs access; approvers read it before deciding.
- **Two approvers for read-write:** Read-write grants (raw TCP only) need two different approvers.
- **Optional auto-approve:** A resource can auto-approve read-only grants from one named vendor, up to a set duration.
- **Revoke in milliseconds:** Revoking a grant closes its live connections; "Cut all access" on a site revokes every approved grant.
- **Portal and CLI:** Approve in the web portal (`/portal/` on any relay with the bridge on) or with `tunr grants`.

## 🗄️ Read-only Postgres
- **Three layers:** a `SELECT`-only login role (`tunr connect pg-role` prints the SQL), read-only sessions with a 30 s statement timeout, and a statement classifier in the connector. Writes fail with SQLSTATE `42501`.
- **No credentials for agents:** The connector logs in with its own stored credential. The agent's DSN only unlocks its own local port.
- **Limits:** Rows per query, bytes and queries per second, set per resource and narrowed per grant.

## 🔒 Transport
- **End-to-end TLS 1.3** between `tunr access` and the connector, pinned to the connector's certificate fingerprint. The relay forwards ciphertext. It does not protect against an actively compromised relay — see [bridge/security.md](bridge/security.md).
- **Short-lived credentials:** Grant tokens ≤15 min (Ed25519-signed by the relay, verified by the connector), agent tokens ≤24 h, rotatable site tokens.

## 📋 Audit & notifications
- **Audit log:** Grant requests and decisions, every session with byte counts, and a summary of every query (command, tables, rows, duration, blocked). Query text only if the customer turns it on. CSV export.
- **E-mail and webhooks:** Grant events by e-mail to approvers and as HMAC-signed webhooks (for Slack bridges, SIEMs, etc.).

## 🤖 Agents (vendor side)
- **Agent identities:** `tunr agent create` / `tunr agent token` — separate from human accounts; customers see which agent asks.
- **CLI and MCP:** `tunr catalog`, `tunr access request`, `tunr access` from scripts; `tunr_list_resources`, `tunr_request_access`, `tunr_access_status`, `tunr_open_access`, `tunr_close_access` from `tunr mcp`.
- **Hosted agents (early access):** `tunr agent deploy` runs an agent on tunr, private, woken by cron (UTC) or signed webhooks, with its identity issued automatically and an optional `/data` volume. Off on tunr.sh until egress and CPU quotas are enforced; self-hosted relays enable it with `TUNR_BRIDGE_HOSTED_AGENTS=1`.

# Tunnels

The original tunr: expose a local port with a public HTTPS URL.

## 🚀 Core Tunneling
- **Zero Config:** Share your local server with a public URL in `< 3 seconds`. No configs, no signup required for basic usage (`tunr share --port 3000`).
- **Auto-HTTPS:** Every tunnel gets a secure, valid Let's Encrypt / Cloudflare HTTPS certificate out of the box.
- **WebSocket & HMR:** Flawless support for WebSockets and Hot Module Replacement (React, Vue, Vite, Next.js).
- **Custom Subdomains:** Claim your own permanent endpoints (e.g., `myapp.tunr.sh`) with a Pro account.

## 🛠 Developer Experience (DX)
- **Multi-Platform Single Binary:** Runs natively on macOS, Linux, and Windows with zero dependencies. No Node.js or Python required.
- **Local HTTP Inspector:** A built-in, beautifully crafted dashboard to inspect incoming requests, headers, and payloads in real-time.
- **Request Replay:** Easily replay intercepted requests from the CLI (`tunr replay <id>`) or export them as `curl` commands.
- **Secure Secret Management:** Auth tokens are securely stored in the native OS Keychain—no insecure plaintext files.

## 🤖 AI & Ecosystem
- **MCP Server Support:** Native Model Context Protocol (MCP) server. Claude Desktop and Cursor IDE can automatically spin up tunnels and inspect requests directly from chat!
- **AI-Friendly CLI:** The `--json` flag formats all outputs in machine-readable JSON for seamless automation. 
- **VS Code Extension:** Control your tunnels right from your editor's status bar and sidebar.

## 💼 Vibecoder Client Demo Features
Taking client presentations to the next level. Tunr proxy dynamically enhances your local app for safe, impressive client demos.

- **Snapshot / Freeze Mode (`--freeze`):** The proxy caches successful responses. If your local server crashes mid-presentation, the proxy falls back to the cache so the client never notices an error.
- **Demo Mode (`--demo`):** A read-only proxy layer. Blocks unsafe mutating requests (`POST`, `PUT`, `PATCH`, `DELETE`) and messages the visitor sends over WebSockets, and returns a mocked success JSON. GraphQL queries still go through; `--demo-allow` / `--demo-block` cover app-specific cases. Your clients can click "Submit Order" without actually messing up your local database!
- **Feedback & Error Widget (`--inject-widget`):** Injects a floating 💬 Feedback button and a JS error catcher into the HTML. Clients type a note; it arrives in your terminal with the page URL and screen size, and uncaught JavaScript errors are logged there too.
- **Auto-Login (`--auto-login`):** Bypasses auth screens for clients by automatically injecting required session Cookies or Headers into the proxy stream. `tunr share --auto-login "Cookie: session=demo"`

## 🌐 Multi-Region Routing
- **Today:** one EU relay serves all traffic. The `--region` flag is accepted as a hint for when more regions exist.

## 🔌 Raw TCP / UDP / TLS Tunnels (experimental)
- **Transport:** raw bytes are carried over a WebSocket at `wss://<subdomain>.tunr.sh/tunnel/tcp` (binary frames). The relay has no raw TCP/UDP port yet, so native clients (`psql`, `ssh`, game clients) can't connect directly.
- **TLS:** `tunr tls` forwards a local TLS port over the same transport. It is **not** SNI passthrough; the relay terminates TLS for `*.tunr.sh`.
- **No IP allowlist:** `--allow-ip` isn't enforced on raw tunnels and returns an error.
- **CLI:** `tunr tcp --port 5432`, `tunr udp --port 53`, `tunr tls --port 8443`

## 🛡️ Tunnel Security
- **Password Protection:** HTTP Basic Authentication with `--password` flag.
- **Bearer Token Auth:** Simple API key-style protection with `--auth-token`.
- **IP Whitelisting:** CIDR-based access control with `--allow-ip`.
- **Auto-Expiry:** TTL-based tunnel expiration with `--ttl 1h`.

## 🔧 Header Manipulation
- **Add Headers:** Inject custom headers on the fly (`--header-add`).
- **Replace Headers:** Override incoming headers (`--header-replace`).
- **Remove Headers:** Strip sensitive headers before forwarding (`--header-remove`).
- **Forwarded Headers:** Inject `X-Forwarded-For` and `X-Original-URL` automatically.
- **CORS Preflight:** Allow cross-origin requests with `--cors-origin`.

## 📱 Quick Sharing
- **QR Code Generation:** Instantly generate scannable QR codes for tunnel URLs (`--qr`).
- **Machine-Readable JSON Output:** All commands support `--json` for CI/CD integration.

## 🐳 Docker & Self-Hosting (New in v0.4.0)
- **Docker Image:** Run tunr from a ~15MB Alpine container: `docker run tunr share -p 3000`
- **Docker Compose Stack:** Full self-hosted deployment with relay + caddy + postgres.
- **Self-Hosting Guide:** Complete docs for running your own relay on any VPS.
- **Systemd/Launchd Service:** `tunr service install --port 3000` for auto-start on boot.

## 📊 Observability (New in v0.4.0)
- **Prometheus Metrics:** `/metrics` endpoint on port 19842 with `tunr_requests_total`, `tunr_active_tunnels`, `tunr_bytes_transferred`.
- **Health Probes:** `/healthz` and `/readyz` endpoints for Kubernetes integration.
- **Connection Draining:** Graceful shutdown waits for in-flight requests before closing.

## 🔀 Multi-Tunnel Config (New in v0.4.0)
- **Named Tunnels:** Define multiple tunnels in `.tunr.json` and start them all at once.
- **`tunr up`:** Start all tunnels from your project config file.
- **`tunr down`:** Stop all running daemon tunnels.
- **Mixed Protocols:** Run HTTP, TCP, UDP, and TLS tunnels simultaneously.

## 🌐 Corporate Proxy Support (New in v0.4.0)
- **HTTP Proxy:** `tunr share -p 3000 --proxy http://proxy:8080`
- **Environment Variables:** Respects `HTTP_PROXY`, `HTTPS_PROXY`, `NO_PROXY` automatically.
- **SOCKS5 Support:** Works with SOCKS5 proxies for corporate firewalls.

---

# SDK Documentation

## Python SDK (`pip install tunr-sdk`)

### Tunnel Creation
```python
from tunr import TunrClient, TunnelOptions
client = TunrClient()
tunnel = client.share(port=3000)
```

### TCP / UDP / TLS Tunnels
```python
tcp_tunnel = client.tcp(port=5432)
udp_tunnel = client.udp(port=53)
tls_tunnel = client.tls(port=8443)
```

### With Options
```python
opts = TunnelOptions(
    subdomain="myapp",
    auth_token="secret",
    allow_ips=["10.0.0.0/8"],
    qr=False,
    password="demo123",
    freeze=True,
    inject_widget=True,
    x_forwarded_for=True,
    cors_origins=["https://example.com"],
    proxy="http://proxy:8080",
    ttl="2h",
)
tunnel = client.share(port=8080, opts=opts)
```

### Inspect Requests
```python
requests = client.get_requests(tunnel.subdomain)
for req in requests:
    print(f"{req['method']} {req['path']} - {req['status_code']}")
```

### Replay Request
```python
client.replay_request(subdomain, requests[0]['id'], port=3000)
```

### Observability
```python
metrics = client.get_metrics()    # Prometheus text format
health = client.health_check()    # {"status": "ok", ...}
```

## Node.js SDK (`npm install @tunr/cli`)

### Tunnel Creation
```typescript
import { TunrClient } from '@tunr/cli'
const client = new TunrClient()
const tunnel = await client.share(3000)
```

### TCP / UDP / TLS Tunnels
```typescript
const tcpTunnel = await client.tcp(5432)
const udpTunnel = await client.udp(53)
const tlsTunnel = await client.tls(8443)
```

### With Options
```typescript
const tunnel = await client.share(8080, {
  subdomain: 'myapp',
  authToken: 'secret',
  allowIps: ['10.0.0.0/8'],
  password: 'demo123',
  freeze: true,
  injectWidget: true,
  xForwardedFor: true,
  corsOrigins: ['https://example.com'],
  proxy: 'http://proxy:8080',
  ttl: '2h',
})
```

### Event-Based Lifecycle
```typescript
tunnel.on('ready', () => console.log('Tunnel live'))
tunnel.on('error', (err) => console.error('Error:', err))
tunnel.on('exit', () => console.log('Tunnel closed'))
await tunnel.close()
```

### Inspect Requests
```typescript
const requests = await client.getRequests(tunnel.subdomain)
for (const req of requests) {
  console.log(`${req.method} ${req.path} - ${req.response?.status}`)
}
```

### Replay Request
```typescript
await client.replayRequest(subdomain, requests[0].id, 3000)
```

### Observability
```typescript
const metrics = await client.getMetrics()    // Prometheus text
const health = await client.healthCheck()    // {status: "ok", ...}
```
