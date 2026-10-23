<div align="center">

<br/>

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="assets/logo-wordmark-dark.png" />
  <source media="(prefers-color-scheme: light)" srcset="assets/logo-wordmark-light.png" />
  <img src="assets/logo-wordmark-light.png" alt="tunr" width="200" />
</picture>

<br/><br/>

**Connect AI agents to your customers' on-prem data.**
Read-only, time-boxed, approved by the customer, and on the record — through one outbound-only connector.

[![Release](https://img.shields.io/github/v/release/tunr-dev/tunr?color=7c3aed)](https://github.com/tunr-dev/tunr/releases)
[![CLI: Apache 2.0](https://img.shields.io/badge/CLI-Apache--2.0-7c3aed.svg)](LICENSE)
[![Relay: PolyForm Shield](https://img.shields.io/badge/relay-PolyForm%20Shield-6b7280.svg)](relay/LICENSE)
[![Go Version](https://img.shields.io/badge/go-1.26+-00add8)](go.mod)
[![CI](https://github.com/tunr-dev/tunr/actions/workflows/ci.yml/badge.svg)](https://github.com/tunr-dev/tunr/actions/workflows/ci.yml)
[![Stars](https://img.shields.io/github/stars/tunr-dev/tunr?style=flat&color=7c3aed)](https://github.com/tunr-dev/tunr/stargazers)

[tunr.sh](https://tunr.sh) · [Live demo](https://app.tunr.sh/demo) · [Docs](https://tunr.sh/docs/) · [Dashboard](https://app.tunr.sh) · [Discussions](https://github.com/tunr-dev/tunr/discussions) · [Contributing](docs/CONTRIBUTING.md)

</div>

```bash
# Customer IT, once — inside their network, outbound HTTPS only:
$ tunr connect --relay https://bridge.tunr.sh --site istanbul-office --relay-key ed25519:… --token-stdin --setup-only   # paste the site token
$ tunr connect resource add erp-db --kind postgres --target 10.0.0.5:5432 --database logo
$ tunr connect pg-role --database logo --tables invoices,stock --resource erp-db > pg-role.sql   # for the DBA

# The vendor's agent — anywhere:
$ tunr access request acme/erp-db --ttl 2h --reason "Reconcile October invoices"
  pending  grt_…  acme/erp-db (ro, 2h0m0s)            # the customer approves in the portal
$ tunr access acme/erp-db --port 15432
postgresql://agent:…@127.0.0.1:15432/db?sslmode=disable
$ psql "$DSN" -c "update invoices set amount = 0"
ERROR:  tunr bridge: UPDATE is not allowed on a read-only bridge
```

Or let the agent do it: `tunr_list_resources`, `tunr_request_access`,
`tunr_open_access` are MCP tools.

---

## Why

AI vendors increasingly sell agents that need data living inside their
customers' networks — on-prem ERPs, ledgers, file servers. Today each vendor
writes its own on-prem agent or asks for a VPN, and every deal waits weeks in
a security review, because the customer can't see or limit what the agent
touches.

tunr Bridge is the standard way across. The customer installs one connector
that only connects out, exposes named resources, and approves each agent,
resource and time window — with a reason they can read. The agent never sees a
database password; databases are read-only in three layers; the stream is
end-to-end encrypted to the customer's connector; every query lands in the
customer's audit log; one click cuts everything. Once a customer has the
connector for one vendor, the next vendor is a link and an approval away.

tunr also hosts the agents themselves (private, woken by cron or signed
webhooks, with their identity issued automatically), still deploys the small
apps agents build, and still has the free 3-second tunnel it started as.

---

## Install

```bash
# macOS / Linux — Homebrew
brew install tunr-dev/tap/tunr

# macOS / Linux — install script
curl -fsSL https://raw.githubusercontent.com/tunr-dev/tunr/main/install.sh | sh

# Go
go install github.com/tunr-dev/tunr/cmd/tunr@latest

# From source
git clone https://github.com/tunr-dev/tunr.git && cd tunr && make build
```

A single static binary. macOS, Linux and Windows, amd64 and arm64, no runtime
dependencies. Verify it with `tunr doctor`.

**SDKs** — for driving tunnels from code rather than the shell:

```bash
pip install tunr          # Python
npm install @tunr/cli     # Node.js
```

---

## Agent Bridge

```
 vendor's agent ──► tunr access ──(TLS 1.3, pinned)──────────────► connector ──► customer's database
 (anywhere)        127.0.0.1:port      ▲ relay sees ciphertext ▲     (outbound only)    (read-only role)
```

| | |
|---|---|
| **Connector** | `tunr connect` runs inside the customer's network as a service, dials out only, and serves named resources. Their addresses never leave the machine. |
| **Grants** | Agent × resource × mode × duration, requested with a reason, approved by the customer (read-write needs two different approvers). They expire on their own. |
| **Read-only databases** | A `SELECT`-only role, read-only sessions and a statement classifier; writes fail with SQLSTATE 42501. Row, byte and rate limits per grant. |
| **No credentials for agents** | The connector logs in itself; the agent's DSN only unlocks its own local port. |
| **End-to-end TLS** | Between `tunr access` and the connector, pinned to the connector's certificate. |
| **Audit** | Every grant decision, session and query summary, exportable as CSV. |
| **Cut access** | Revoke closes live connections in milliseconds; the site kill switch revokes everything. |
| **Hosted agents** *(early access)* | `tunr agent deploy` runs an agent on tunr, private, woken by cron or signed webhooks, with its identity issued automatically and a `/data` volume. Off on tunr.sh until egress and CPU quotas land; self-hosters enable it with `TUNR_BRIDGE_HOSTED_AGENTS=1`. |
| **Dashboard** | [app.tunr.sh](https://app.tunr.sh): approvals inbox, sites, resources, grants and their queries, audit + CSV export, agents and tokens, webhooks, org settings. Self-hosted relays ship the same flows as a built-in portal at `/portal/`. |

What it guarantees, how, and what it doesn't: **[docs/bridge/security.md](docs/bridge/security.md)**.
Getting started: [customer](docs/bridge/quickstart-customer.md) · [vendor](docs/bridge/quickstart-vendor.md) · protocol: [docs/protocol/bridge.md](docs/protocol/bridge.md).

> **Status: preview.** Live on tunr.sh and end-to-end tested (two docker
> suites in CI); expect sharp edges. Postgres and raw TCP resources today;
> MySQL/MSSQL later.

---

## Cloud — deploy & host

<p align="center">
  <img src="assets/demo.gif" alt="An agent calls tunr_deploy; the build streams and the app goes live at https://standup.tunr.sh" width="800" />
</p>

```bash
tunr login                        # magic-link, token goes to your OS keychain
tunr deploy --name my-app         # build + run; prints the live URL
tunr apps                         # list your apps
tunr apps logs my-app --follow    # stream build + runtime logs
tunr apps delete my-app           # remove it, free the subdomain
```

| | |
|---|---|
| **Build** | Nixpacks auto-detect — Node, Python, Go, Ruby, Rust, PHP, Java. A `Dockerfile` is used if present. |
| **Isolation** | Every app runs in its own gVisor sandbox with CPU, memory and disk quotas. |
| **Scale to zero** | Idle → paused with its memory reclaimed (~20 MB resident), then stopped. Wake p50 is ~150 ms. |
| **Secrets** | `.env` files are never uploaded. Pass them with `--env KEY=VALUE`. |
| **Stays up** | Close your laptop. The app keeps serving. |

```bash
tunr deploy --name sprint --port 3000 --env DATABASE_URL=postgres://…
```

> **Status: preview.** Deploy works end to end and is what the MCP tools drive.
> Role-based sharing, per-app SQLite and `tunr rollback` are **not built yet** —
> see [Roadmap](#roadmap) for what's real and what isn't.

---

## Agent-native (MCP)

The CLI and the MCP server are two faces of the same control plane. Anything
you can do in a terminal, your agent can do in a tool call.

**Claude Code:**

```bash
claude mcp add tunr -- tunr mcp
```

**Claude Desktop** (`~/.claude/claude_desktop_config.json`) **/ Cursor** (`.cursor/mcp.json`):

```json
{
  "mcpServers": {
    "tunr": { "command": "tunr", "args": ["mcp"] }
  }
}
```

### Tools

| Tool | What it does |
|---|---|
| `tunr_deploy` | Build & host a directory on the tunr cloud, return the live URL |
| `tunr_list_apps` | List deployed apps with URL and status |
| `tunr_app_logs` | Read an app's build + runtime logs (for debugging a bad deploy) |
| `tunr_delete_app` | Delete an app and free its subdomain |
| `tunr_share` | Open a temporary public tunnel to a running local port |
| `tunr_status` | List active tunnels |
| `tunr_inspect` | List HTTP requests captured by a tunnel |
| `tunr_replay` | Replay a captured request against your local server |
| `tunr_stop` | Close a tunnel |

`tunr_deploy` and `tunr_share` are deliberately distinct, and the tool
descriptions say so: *deploy* means "host this, it should outlive my laptop",
*share* means "expose what's running on :3000 right now". Cloud tools need
`tunr login` first.

---

## Client demos — for freelancers and vibecoders

Showing a client something that still runs on your laptop? Three flags make the
free tunnel safe to hand to someone who isn't a developer.

<p align="center">
  <img src="assets/client-demo.gif" alt="tunr share with --demo, --freeze and --inject-widget: a client's delete is intercepted, a crash is hidden, feedback arrives in the terminal" width="360" />
</p>

```bash
tunr share -p 3000 --demo --freeze --inject-widget --qr
```

| Flag | What the client sees | What actually happens |
|---|---|---|
| `--demo` | Buttons work — "Order deleted ✓" | `POST`/`PUT`/`PATCH`/`DELETE` are answered by tunr with a fake success (JSON bodies are echoed back) and never reach your app. GraphQL queries still go through. Messages the visitor sends over WebSockets are dropped. Tune it with `--demo-allow` / `--demo-block` ([details](docs/VIBECODER_FEATURES.md#--demo-safe-read-only-mode)). |
| `--freeze` | The page, as it was a second ago | Your dev server crashed or returned a 5xx; tunr serves the last good response (`X-Tunr-Freeze-Cache: HIT`) while you fix it. |
| `--inject-widget` | A 💬 **Feedback** button on every page | Their message, page URL and screen size land in your terminal. No code changes — tunr injects it into the HTML. |
| `--auto-login` | Already signed in to a demo account | tunr adds your session cookie (or `Bearer …` header) to every request: `--auto-login "session=demo-token"`. |
| `--qr` · `--password` · `--ttl 1h` | A QR code to open it on their phone; a password; a link that expires | Access control that takes one flag each. |

---

## Tunnels

Still free, still unlimited, still 3 seconds.

```bash
tunr share --port 3000
#  🚀 https://abc1x2y3.tunr.sh
```

HTTP/HTTPS with WebSocket (HMR works), plus experimental raw **TCP**, **UDP**
and **TLS** tunnels — all multiplexed over one connection.
Traffic is served from a single EU relay today; more regions are planned.

<details>
<summary><b>Access control</b> — password, bearer token, IP whitelist, TTL, QR</summary>

<br/>

```bash
# Basic auth (user optional)
tunr share -p 8080 --password "secret"
tunr share -p 8080 --password "client:secret"

# Bearer token — Authorization: Bearer <token> or ?token=<token>
tunr share -p 3000 --auth-token "my-super-secret-key"

# IP whitelist (CIDR)
tunr share -p 3000 --allow-ip "203.0.113.0/24"
tunr share -p 3000 --allow-ip "10.0.0.0/8,172.16.0.0/12"

# Auto-expire — the tunnel closes itself
tunr share -p 3000 --ttl 1h30m

# QR code, for opening it on a phone
tunr share -p 3000 --qr
```

</details>

<details>
<summary><b>Routing & headers</b> — path routing, regions, header rewriting, CORS, proxies</summary>

<br/>

```bash
# Path routing — one public URL, several local ports
tunr share --route /=3000 --route /api=8080

# Region hint — accepted today, takes effect once more regions launch
# (tunr currently runs a single relay in the EU)
tunr share --port 3000 --region ams

# Header rewriting
tunr share -p 3000 --header-add "X-Debug: true"
tunr share -p 3000 --header-replace "Host: internal.local"
tunr share -p 3000 --header-remove "X-Powered-By"

# Forwarded headers — X-Forwarded-For (real client IP), X-Original-URL
tunr share -p 3000 --x-forwarded-for --original-url

# CORS preflight without touching your server
tunr share -p 3000 --cors-origin "https://myapp.com"

# Behind a corporate proxy
tunr share -p 3000 --proxy http://proxy:8080

# Custom domain (Pro)
tunr share -p 3000 --domain demo.client.com
```

</details>

<details>
<summary><b>TCP / UDP / TLS</b> — experimental raw tunnels</summary>

<br/>

```bash
tunr tcp --port 5432
tunr udp --port 53
tunr tls --port 8443   # a local TLS port over the same raw transport
```

**Experimental, and narrower than the names suggest.** The relay exposes a raw
tunnel only as a WebSocket stream at `wss://<subdomain>.tunr.sh/tunnel/tcp`
(binary frames carry the bytes). There is no raw TCP/UDP port on the relay yet,
so `psql`, `ssh` or a game client can't connect directly — you need a client
that speaks that WebSocket. `tunr tls` is **not** SNI passthrough: the relay
terminates TLS for `*.tunr.sh`. `--allow-ip` isn't enforced on raw tunnels, so
it returns an error rather than silently leaving the port open.

</details>

<details>
<summary><b>Daemon, service & multi-tunnel</b></summary>

<br/>

```bash
# Background daemon
tunr start --port 3000
tunr status
tunr stop

# Several tunnels from .tunr.json
tunr up
tunr down

# Install as a system service (systemd / launchd) — starts on boot
tunr service install --port 3000
tunr service status
tunr service uninstall
```

</details>

<details>
<summary><b>Inspect & replay</b> — the local HTTP inspector</summary>

<br/>

```bash
tunr open           # dashboard at http://localhost:19842
tunr logs --follow  # stream requests in the terminal
tunr replay <id>    # re-send a captured request to your local server
```

Live request/response stream, headers, body, timing, one-click replay, export
as a `curl` command. Everything stays on your machine.

</details>

<details>
<summary><b>Full CLI reference</b></summary>

<br/>

| Command | Description |
|---------|-------------|
| **Cloud** | |
| `tunr deploy [dir]` | Build & host a project; `--name`, `--port`, `--env KEY=VAL` |
| `tunr apps` | List your cloud apps |
| `tunr apps logs <name>` | Stream logs; `--follow`, `--tail N` |
| `tunr apps delete <name>` | Delete an app |
| **Tunnels** | |
| `tunr share -p PORT` | Expose a local port over HTTPS |
| `tunr share -p PORT -s NAME` | Custom subdomain (Pro) |
| `tunr share --route /PATH=PORT` | Map URL paths to local ports |
| `tunr share -p PORT --password PASS` | Basic authentication |
| `tunr share -p PORT --ttl 1h` | Auto-close after a duration |
| `tunr share -p PORT --demo` | Read-only demo mode |
| `tunr share -p PORT --freeze` | Serve last-good response on crash |
| `tunr share -p PORT --inject-widget` | Inject the feedback widget |
| `tunr share -p PORT --auto-login "Cookie: s=demo"` | Auto-inject an auth cookie |
| `tunr share -p PORT --domain HOST` | Custom domain |
| `tunr share -p PORT --qr` | Print a QR code for the URL |
| `tunr share -p PORT --auth-token TOKEN` | Bearer token protection |
| `tunr share -p PORT --allow-ip CIDR` | IP whitelist |
| `tunr share -p PORT --header-add/-replace/-remove` | Rewrite headers |
| `tunr share -p PORT --x-forwarded-for --original-url` | Proxy headers |
| `tunr share -p PORT --cors-origin ORIGIN` | CORS preflight |
| `tunr share -p PORT --proxy URL` | HTTP/SOCKS5 proxy |
| `tunr share -p PORT --region ams` | Relay region hint (single EU relay today) |
| `tunr share -p PORT --json` | JSON output for CI |
| `tunr tcp -p PORT` / `tunr udp -p PORT` / `tunr tls -p PORT` | Raw tunnels over WebSocket (experimental) |
| `tunr up` / `tunr down` | Start/stop everything in `.tunr.json` |
| `tunr start` / `tunr stop` / `tunr status` | Daemon mode |
| `tunr service install\|status\|uninstall` | System service |
| **Everything else** | |
| `tunr login` / `tunr logout` | Authentication |
| `tunr open` / `tunr logs` / `tunr replay <id>` | Inspector |
| `tunr mcp` | Start the MCP server (stdio) |
| `tunr config init` / `tunr config show` | `.tunr.json` |
| `tunr doctor` | Diagnose your setup |
| `tunr update` / `tunr uninstall` / `tunr version` | Maintenance |

Global flags: `--relay URL` (point at your own relay), `--verbose`.

</details>

---

## SDKs

<details>
<summary><b>Python</b> — <code>pip install tunr</code></summary>

<br/>

```python
from tunr import TunrClient, TunnelOptions

client = TunrClient()

tunnel = client.share(port=3000)
print(tunnel.public_url)

db_tunnel  = client.tcp(port=5432)
dns_tunnel = client.udp(port=53)
tls_tunnel = client.tls(port=8443)

opts = TunnelOptions(
    subdomain="myapp",
    password="demo123",
    allow_ips=["10.0.0.0/8"],
    freeze=True,
    inject_widget=True,
    proxy="http://proxy:8080",
    ttl="2h",
)
tunnel = client.share(port=8080, opts=opts)

requests = client.get_requests(tunnel.subdomain)
client.replay_request(tunnel.subdomain, requests[0]["id"], port=3000)

metrics = client.get_metrics()   # Prometheus text
health  = client.health_check()  # {"status": "ok"}

tunnel.close()
```

</details>

<details>
<summary><b>Node.js</b> — <code>npm install @tunr/cli</code></summary>

<br/>

```typescript
import { TunrClient } from '@tunr/cli'

const client = new TunrClient()

const tunnel = await client.share(3000)
console.log(tunnel.publicUrl)

const dbTunnel  = await client.tcp(5432)
const dnsTunnel = await client.udp(53)
const tlsTunnel = await client.tls(8443)

const appTunnel = await client.share(8080, {
  subdomain: 'myapp',
  password: 'demo123',
  allowIps: ['10.0.0.0/8'],
  freeze: true,
  injectWidget: true,
  proxy: 'http://proxy:8080',
  ttl: '2h',
})

tunnel.on('ready', () => console.log('Tunnel live'))
tunnel.on('error', (err) => console.error(err))
tunnel.on('exit',  () => console.log('Tunnel closed'))

const requests = await client.getRequests('myapp')
await client.replayRequest('myapp', requests[0].id, 3000)

await tunnel.close()
```

</details>

---

## Configuration (`.tunr.json`)

```bash
tunr config init
```

```json
{
  "$schema": "https://tunr.sh/schema/.tunr.schema.json",
  "port": 3000,
  "inspectorEnabled": true,
  "dashboardPort": 19842,
  "mcp": { "enabled": true }
}
```

---

## Architecture

```
Browser ──▶ relay.tunr.sh ──┬── [WebSocket] ──▶ tunr CLI ──▶ localhost:PORT   (tunnel)
                            │
                            └── control plane ──▶ tunr-runner ──▶ gVisor sandbox   (cloud app)
                                     │
                                Postgres + route cache
```

**Tunnels.** The CLI starts a local proxy with an embedded inspector and opens
one WebSocket to the relay. The relay issues a `*.tunr.sh` subdomain and
terminates HTTPS. HTTP, WebSocket, TCP, UDP and TLS all multiplex over that
single connection — a typed message discriminator routes them.

**Cloud.** `tunr deploy` uploads a tarball to the control plane, which hands it
to `tunr-runner`. The runner builds with Nixpacks and runs the result in a
gVisor sandbox with cgroup quotas. The relay routes the subdomain straight at
the container, waking it if it's asleep.

**Scale to zero.** Apps move `HOT → WARM → STOPPED` on an idle timer. WARM is a
cgroup memory reclaim followed by a pause, which cuts real memory cost by ~55%
while keeping wake latency around 150 ms. Health-check probes are answered at
the edge so a monitored app can still fall asleep; search crawlers get the real
page but can't keep the app awake.

**Observability.** Prometheus metrics at `/metrics`, K8s probes at `/healthz`
and `/readyz` on the inspector port.

**Self-hosting.** `docker-compose.yml` runs the tunnel stack (relay + Caddy +
Postgres); `docker-compose.runner.yml` adds the cloud runner. Point the CLI at
it with `--relay https://tunnel.yourcompany.com` or `TUNR_RELAY_URL`. See
[docs/SELF_HOSTING.md](docs/SELF_HOSTING.md).

---

## Security

- Agent Bridge: default deny, read-only databases in three layers, no credentials for agents, end-to-end TLS to the connector, per-org isolation, full audit — details and limits in [docs/bridge/security.md](docs/bridge/security.md)
- Auth tokens live in the **OS keychain**, never in a dotfile
- All relay traffic over **TLS 1.3**. HTTP tunnels terminate TLS at the relay, like any hosted tunnel; self-host the relay (`--relay`) if that matters to you
- Cloud apps run under **gVisor**, not bare containers
- `.env` files are excluded from deploy uploads by default
- The CLI ships no telemetry, no analytics, no phone-home
- Supply chain: `go mod verify` + `govulncheck` in CI, cosign-signed checksums

Found a vulnerability? **Don't open a public issue** — see [SECURITY.md](SECURITY.md).

---

## Roadmap

Honest status. "Preview" means it works but the edges are sharp; "planned"
means there is no code yet.

| | Status |
|---|---|
| **Agent Bridge**: connector, grants, read-only Postgres proxy, end-to-end TLS, MCP tools — live on tunr.sh since v0.8.0 | 🚧 Preview |
| Bridge dashboard ([app.tunr.sh](https://app.tunr.sh)): approvals, sites, agents & tokens, grants with per-query summaries, audit + CSV/JSON export, webhooks, org settings | 🚧 Preview |
| Bridge orgs: members with owner / admin / approver / member roles, vendor↔customer links, two-approver rule for read-write | 🚧 Preview |
| Self-hosted bridge (`docker compose`, built-in `/portal/`, on-demand TLS) | 🚧 Preview |
| **Hosted agents** (`tunr agent deploy`, cron/webhook triggers, `/data` volume) | 🧪 Early access (off on tunr.sh) |
| Bridge: MySQL / MSSQL proxies, HTTP resources, white-label connector for vendors | 📋 Planned |
| Bridge: volume size quotas, egress blocklist for hosted agents | 📋 Planned |
| HTTP/WS tunnels | ✅ Stable |
| Raw TCP / UDP / TLS tunnels (WebSocket clients only, not end-to-end) | 🧪 Experimental |
| Demo features (freeze, demo, widget, auto-login) | ✅ Stable |
| Access control (password, token, IP, TTL) | ✅ Stable |
| Inspector + replay, Prometheus, service install | ✅ Stable |
| Python / Node SDKs | ✅ Stable |
| Self-hosted tunnel relay | ✅ Stable |
| `tunr deploy` + `tunr apps` + logs (v0.6.1+) | 🚧 Preview |
| MCP cloud tools (`tunr_deploy`, `tunr_app_logs`, …) (v0.6.1+) | 🚧 Preview |
| Scale-to-zero (sleep/wake) | 🚧 Preview |
| Self-hosted cloud runner | 🚧 Preview |
| **Role-based sharing** (viewer/commenter/editor, `--org acme.com`) | 📋 Planned |
| Per-app SQLite + snapshots | 📋 Planned |
| `tunr rollback` (code *and* data) | 📋 Planned |
| Multi-region relay (`--region` is accepted; one EU relay today) | 📋 Planned |
| Persistent TCP/UDP ports | 📋 Backlog |
| GUI desktop app | 📋 Backlog |

Comparing tunr's tunnel against ngrok, Cloudflare Tunnel, LocalXpose and
localtunnel: [docs/compare-tunnels.md](docs/compare-tunnels.md).

---

## Contributing

Contributions are welcome — read [CONTRIBUTING.md](docs/CONTRIBUTING.md) first.
Good places to start are issues labelled
[`good first issue`](https://github.com/tunr-dev/tunr/labels/good%20first%20issue)
and [`help wanted`](https://github.com/tunr-dev/tunr/labels/help%20wanted).

```bash
make check      # vet + lint + test + govulncheck
make pre-push   # the above, plus a build
```

The relay is a separate Go module and isn't covered by the Makefile:

```bash
cd relay && go build ./cmd/server && go test ./...
```

---

## Community & support

| | |
|---|---|
| Bugs & feature requests | [GitHub Issues](https://github.com/tunr-dev/tunr/issues) |
| Questions & ideas | [GitHub Discussions](https://github.com/tunr-dev/tunr/discussions) |
| Security reports | [Private advisory](https://github.com/tunr-dev/tunr/security/advisories/new) or [dev@tunr.sh](mailto:dev@tunr.sh) |
| Developer / contributor contact | [dev@tunr.sh](mailto:dev@tunr.sh) |
| Everything else (billing, partnerships, press) | [contact@tunr.sh](mailto:contact@tunr.sh) |

Please follow our [Code of Conduct](CODE_OF_CONDUCT.md).

---

## License

This repository is **dual-licensed by directory**:

| Path | Licence | |
|---|---|---|
| `cmd/`, `internal/`, `sdk/` — the CLI and SDKs | [Apache-2.0](LICENSE) | OSI-approved open source. Use it, fork it, ship it commercially. |
| `relay/` — relay, control plane, runner | [PolyForm Shield 1.0.0](relay/LICENSE) | Source-available, **not** open source. Read it, modify it, run your own instance — but don't use it to compete with tunr. |

Running the whole stack yourself, for yourself or inside your company, is
allowed under both. See [NOTICE](NOTICE) for the exact boundary.

---

<div align="center">

**[tunr.sh](https://tunr.sh)** · [Docs](https://tunr.sh/docs/) · [Twitter/X](https://x.com/vural_met)

Built with 💜 in Go

</div>
