# Running Relay on Server

Relay is the server-side component that routes traffic for tunnels opened with `tunr share` and, when enabled, runs the Agent Bridge (connectors, grants, audit and the built-in portal). This document summarizes how to build and run relay in production. For a Docker Compose setup, see [SELF_HOSTING.md](SELF_HOSTING.md).

## Requirements

- Go 1.26+
- Required env: `TUNR_JWT_SECRET` (32+ chars), `TUNR_DOMAIN=tunr.sh`, `PORT=8080`
- Optional: `DATABASE_URL` (PostgreSQL; runs in-memory when unset). **Required for the Agent Bridge**, which has no in-memory mode.
- Agent Bridge only: the schema from `relay/migrations/` (see step 3)

## Steps

### 1. Get the relay source on the server

The relay lives in `relay/` of this repository (its own Go module, PolyForm Shield licensed):

```bash
git clone https://github.com/tunr-dev/tunr.git /opt/tunr/src
```

### 2. Build

```bash
cd /opt/tunr/src/relay
CGO_ENABLED=0 go build -trimpath -ldflags="-w -s" -o /usr/local/bin/tunr-relay ./cmd/server
```

### 3. Environment variables

Set these in `/opt/tunr/.env` (or in the systemd `EnvironmentFile`):

```
TUNR_JWT_SECRET=...   # minimum 32 chars (must match dashboard setup)
TUNR_DOMAIN=tunr.sh
PORT=8080
DATABASE_URL=         # optional for tunnels, required for the Agent Bridge
```

#### Agent Bridge (optional, preview)

Add these to the same file to turn the bridge on. With `TUNR_BRIDGE_ENABLED`
unset nothing bridge-related is mounted.

```
TUNR_BRIDGE_ENABLED=1
TUNR_GRANT_SIGNING_KEY=...        # openssl rand -base64 32 — Ed25519 seed; keep it stable
TUNR_BRIDGE_PUBLIC_URL=https://relay.tunr.sh   # default https://relay.<TUNR_DOMAIN>
TUNR_BRIDGE_PORTAL_URL=           # base of links in grant e-mails; default https://app.<TUNR_DOMAIN>/bridge
RESEND_API_KEY=                   # portal sign-in links + grant e-mails
RESEND_FROM=                      # e.g. "tunr <noreply@yourdomain>"
TUNR_METRICS_TOKEN=               # optional; enables /api/v1/bridge/metrics and /api/v1/density
TUNR_BRIDGE_HOSTED_AGENTS=        # optional, early access; 1 = allow `tunr agent deploy`
```

- The relay refuses to start with the bridge on and no `DATABASE_URL` or no
  `TUNR_GRANT_SIGNING_KEY` (outside `TUNR_DEV_MODE`, which uses an ephemeral
  key that breaks connectors on every restart).
- Connectors pin the public half of the signing key at install time. Rotating
  it means re-installing every connector. Whoever holds it can mint grants
  that connectors accept, so treat it like the JWT secret.
- Without `RESEND_API_KEY`, grant e-mails are only logged and nobody can sign
  in to the portal.
- Hosted agents also need a runner (`RUNNER_URL`, `RUNNER_SECRET`) started
  with `TUNR_AGENT_BINARY` pointing at a static Linux `tunr` binary on the
  host. Their egress and CPU quotas are not enforced yet.

Apply the schema before starting the relay with the bridge on. The relay does
not run migrations; every file is re-runnable, so apply all of them in order
after each upgrade (`005`–`009` are the bridge tables):

```bash
for f in /opt/tunr/src/relay/migrations/*.sql; do
  psql "$DATABASE_URL" -v ON_ERROR_STOP=1 -f "$f"
done
```

### 4. Systemd service

```bash
sudo tee /etc/systemd/system/tunr-relay.service << 'EOF'
[Unit]
Description=tunr Relay Server
After=network.target

[Service]
User=tunr
WorkingDirectory=/opt/tunr
EnvironmentFile=/opt/tunr/.env
ExecStart=/usr/local/bin/tunr-relay
Restart=always
RestartSec=5s
StandardOutput=journal
StandardError=journal
SyslogIdentifier=tunr-relay

[Install]
WantedBy=multi-user.target
EOF

sudo systemctl daemon-reload
sudo systemctl enable --now tunr-relay
```

### 5. Verify

```bash
systemctl status tunr-relay
journalctl -u tunr-relay -f -n 30
curl -s http://localhost:8080/api/v1/health
curl -s http://localhost:8080/api/v1/bridge/health   # bridge only
```

With the bridge on, the startup log shows `agent bridge enabled` and the relay
key, and the built-in portal is served at `https://<relay>/portal/`.

Ensure Caddy routes `relay.tunr.sh` and `*.tunr.sh` to port `8080` on this server.
