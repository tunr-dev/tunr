# Self-Hosting tunr

This guide explains how to run your own tunr relay server. This is useful for:
- Organizations that need full data sovereignty
- Air-gapped environments
- Custom domain routing without the tunr.sh cloud
- Running the [Agent Bridge](#enabling-the-agent-bridge-preview) without a third-party relay in the path

## Prerequisites

- A server with a public IP (Hetzner, DigitalOcean, AWS, etc.)
- A domain with DNS access (e.g., `tunnel.yourcompany.com`)
- Docker and Docker Compose installed
- Ports 80 and 443 open

## Quick Start

### 1. Clone the repository
```bash
git clone https://github.com/tunr-dev/tunr.git
cd tunr
```

### 2. Configure DNS

Add a wildcard A record pointing to your server:

```
*.tunnel.yourcompany.com  A  <your-server-ip>
tunnel.yourcompany.com    A  <your-server-ip>
```

### 3. Set environment variables

```bash
cp .env.example .env
# Edit .env with your values:
#   TUNR_DOMAIN=tunnel.yourcompany.com
#   TUNR_JWT_SECRET=$(openssl rand -hex 32)
#   POSTGRES_PASSWORD=<random>
```

Caddy (`caddy/Caddyfile`) gets a certificate per hostname on first use, and
only for names the relay serves (`/api/v1/tls/ask`), so no DNS plugin is
needed. The database schema is applied automatically on the first start; after
an upgrade, re-run the migrations as shown under *Enabling the Agent Bridge*.
The names `relay`, `bridge`, `app`, `www` and `api` are reserved and can't be
used as tunnel subdomains or app names.

### 4. Start the stack

```bash
docker compose up -d
```

This starts:
- **Caddy** — TLS termination with automatic Let's Encrypt certificates
- **Relay** — WebSocket relay server for tunnel traffic
- **PostgreSQL** — Database for user accounts, tunnel history, audit logs

### 5. Connect a client

```bash
tunr share --port 3000 --relay https://tunnel.yourcompany.com
```

Or set it permanently:
```bash
export TUNR_RELAY_URL=https://tunnel.yourcompany.com
tunr share --port 3000
```

## Architecture

```
Internet
  │
  ▼
┌──────────────────────┐
│  Caddy (TLS/HTTPS)   │ ← Wildcard cert: *.tunnel.yourcompany.com
│  :80, :443           │
└──────────┬───────────┘
           │
           ▼
┌──────────────────────┐
│  tunr Relay Server   │ ← WebSocket hub, subdomain routing
│  :8080 (internal)    │
└──────────┬───────────┘
           │
           ▼
┌──────────────────────┐
│  PostgreSQL          │ ← Users, tunnels, audit logs
│  :5432 (internal)    │
└──────────────────────┘
```

## Enabling the Agent Bridge (preview)

The bridge (`tunr connect` / `tunr access`, see [ARCHITECTURE.md](ARCHITECTURE.md#1-agent-bridge-preview))
runs inside the same relay process. It is off unless you turn it on; with it
off no bridge route is mounted and the relay behaves exactly as above.

### 1. Requirements

- **PostgreSQL.** The bridge has no in-memory mode: with
  `TUNR_BRIDGE_ENABLED=1` and no `DATABASE_URL` the relay refuses to start.
- **Migrations.** The relay does not apply migrations itself. Apply every file
  in `relay/migrations/` in order (`001`–`004` are the base schema; `005`–`009`
  add orgs, sites, resources, agents, grants, webhooks, hosted agents and site
  TLS). The files are written to be re-runnable, so apply them all again after
  every upgrade:

  ```bash
  for f in relay/migrations/*.sql; do
    docker compose exec -T postgres psql -v ON_ERROR_STOP=1 -U tunr -d tunr < "$f"
  done
  ```

### 2. Environment

| Variable | Required | Meaning |
|----------|----------|---------|
| `TUNR_BRIDGE_ENABLED` | yes | `1` or `true` turns the bridge on. Any other value leaves it off. |
| `TUNR_GRANT_SIGNING_KEY` | yes | Ed25519 key that signs grant tokens: a base64 32-byte seed (or 64-byte private key). Generate with `openssl rand -base64 32`. |
| `TUNR_BRIDGE_PUBLIC_URL` | usually | URL connectors and agents dial, also used in portal sign-in links. Defaults to `https://relay.<TUNR_DOMAIN>`, so set it if your relay lives elsewhere, e.g. `https://tunnel.yourcompany.com`. |
| `RESEND_API_KEY`, `RESEND_FROM` | for the portal | Sends portal sign-in links and grant e-mails through [Resend](https://resend.com). Without them grant e-mails are only logged, and nobody can sign in to the portal (except with `TUNR_DEV_MODE=1`, which returns the link in the response — development only). |
| `TUNR_BRIDGE_PORTAL_URL` | no | Base of the links in grant e-mails (`<url>/grants/<id>`). Defaults to `https://app.<TUNR_DOMAIN>/bridge`, the hosted dashboard. With the built-in portal, set `https://tunnel.yourcompany.com/portal/#` — links then open the approvals inbox. |
| `TUNR_METRICS_TOKEN` | no | Bearer token that enables `GET /api/v1/bridge/metrics` (and the relay's `/api/v1/density`). Unset, both routes are not mounted. |
| `TUNR_BRIDGE_HOSTED_AGENTS` | no | `1` enables hosted agents (`tunr agent deploy`). Early access — see below. |

**Keep the signing key stable and secret.** Connectors pin the relay's public
key at install time; a new key breaks every installed connector until it is
set up again. Anyone with the key can mint grants that connectors accept.
The relay logs the public key (`ed25519:…`) at startup, and the portal shows
it in each site's install command.

### 3. Portal

Restart the relay. The customer portal and vendor console are built in at
`https://tunnel.yourcompany.com/portal/`: sign in by e-mail, create an
organization, add a site, approve grants, read the audit log. Then follow the
[customer](bridge/quickstart-customer.md) and
[vendor](bridge/quickstart-vendor.md) quick starts with
`--relay https://tunnel.yourcompany.com`.

### 4. Hosted agents (optional, early access)

Hosted agents run on the [cloud layer](#adding-the-cloud-layer-tunr-deploy)
below, so set that up first. Then:

```bash
# relay .env
TUNR_BRIDGE_HOSTED_AGENTS=1

# runner .env — host path of a static (CGO_ENABLED=0) Linux tunr binary,
# mounted read-only into every hosted agent
TUNR_AGENT_BINARY=/opt/tunr/bin/tunr
```

Without `TUNR_BRIDGE_HOSTED_AGENTS=1` the deploy endpoint answers
`503 unavailable`. Egress and CPU quotas for hosted agents are not enforced
yet; only enable this for agents you trust.

## Adding the cloud layer (`tunr deploy`)

Everything above self-hosts **tunnels**. To also run `tunr deploy` — building
and hosting apps on your own box — you need the runner sidecar, which drives
Docker directly.

> **Preview.** This path is newer than the tunnel stack and assumes a Linux
> host you control. It does not work on Docker Desktop for macOS: the density
> levers write to the host cgroup hierarchy.

### 1. Host prerequisites

```bash
sudo ./scripts/host-density.sh     # zram + cgroup v2 soft limits
docker network create --opt com.docker.network.bridge.enable_icc=false tunr-apps
```

`host-density.sh` is not optional. Without zram, `memory.reclaim` has nowhere
to evict to and `memory.high` containment fails outright — an app that should
have been throttled triggers a system-wide OOM instead. Install gVisor
(`runsc`) too, or set the runner to `runc` and accept weaker isolation.

### 2. Start the runner

```bash
docker compose -f docker-compose.runner.yml up -d
```

The runner needs `/sys/fs/cgroup` mounted **rw**, `/proc` with `pid: host`, and
the Docker socket. Without those every density lever silently no-ops — check
for `cgroup levers OFF` in its startup log.

### 3. Point the relay at it

```bash
# .env
RUNNER_URL=http://tunr-runner:9091
RUNNER_SECRET=<shared secret, also set on the runner>
```

Restart the relay, then:

```bash
tunr login --relay https://tunnel.yourcompany.com
tunr deploy --name my-app --relay https://tunnel.yourcompany.com
```

As a rule of thumb, split the builder onto its own machine once builds start
to show up in wake latency: the runner's `--role=builder` / `--role=agent`
flags let you do that with a compose change.

## Systemd (without Docker)

If you prefer running without containers:

```ini
# /etc/systemd/system/tunr-relay.service
[Unit]
Description=tunr Relay Server
After=network-online.target postgresql.service

[Service]
Type=simple
User=tunr
ExecStart=/usr/local/bin/tunr-relay
Restart=always
RestartSec=5
Environment=TUNR_DOMAIN=tunnel.yourcompany.com
Environment=PORT=8080
Environment=DATABASE_URL=postgres://tunr:tunr@localhost:5432/tunr
Environment=TUNR_JWT_SECRET=your-secret-here

[Install]
WantedBy=multi-user.target
```

```bash
sudo systemctl enable --now tunr-relay
```

## Security Notes

- Always use HTTPS in production (Caddy handles this automatically)
- Change `TUNR_JWT_SECRET` to a strong random value
- Consider restricting access with IP whitelisting at the firewall level
- The relay server stores tunnel metadata and audit logs; ensure your PostgreSQL is secured
- Use the PolyForm Shield license terms — self-hosting for internal use is allowed; competing services are not

## Updating

```bash
cd tunr
git pull
docker compose build --no-cache relay
docker compose up -d
```
