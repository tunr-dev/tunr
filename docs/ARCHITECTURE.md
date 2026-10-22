# Tunr Architecture & Under The Hood

This document outlines the high-level architecture of `tunr`, its relay infrastructure, and local binary system.

## 1. Agent Bridge (preview)
The bridge lets a vendor's AI agent reach a resource (today: PostgreSQL or a raw TCP service) inside a customer's network, for a limited time, with the customer's approval. It is built on the same relay and CLI as tunnels but uses its own endpoints, so it never shares a WebSocket with `/tunnel/connect`. On the relay everything is behind `TUNR_BRIDGE_ENABLED=1`; with the flag off no bridge route is mounted.

```
agent ──► tunr access ──WS /bridge/access──► relay ◄──WS /bridge/connect── connector ──► resource
          127.0.0.1:port                  (grants, audit)       (dials out)  (customer network)

          └───────────── TLS 1.3 inside the stream, pinned to the site certificate ─────────────┘
```

- **Connector** (`tunr connect`, `internal/bridge/connector/`) runs inside the customer network and dials the relay's `/bridge/connect` WebSocket outbound with a site token. It announces resource *names* only; target addresses stay in its local `connector.json`. Secrets (site token, database credentials) live in the OS keychain or, on headless servers, in a `0600` file store.
- **Agent side** (`tunr access`, `internal/bridge/access/`) listens on `127.0.0.1` and opens one `/bridge/access` WebSocket per local TCP connection, authenticated with an agent token (`typ: "agent"`, at most 24 h; human JWTs and agent tokens are not interchangeable).
- **Relay** (`relay/internal/bridge/`) checks, before upgrading the WebSocket, that the agent and token are active, the customer has linked the vendor, an approved unexpired grant exists, and the site's connector is online. It then pairs the agent stream with a stream on the connector's WebSocket and forwards bytes (`bridge_open` / `bridge_data` / `bridge_close`). Default deny: no grant, no stream.
- **Grants** bind agent × resource × mode (`ro`/`rw`) × TTL, with a reason the customer reads. Customer approvers decide in the portal or with `tunr grants`; `rw` (raw TCP only) needs two different approvers. Grants can narrow a resource's limits (`max_rows`, `max_bytes`, `qps`), never widen them.
- **Grant JWTs.** For every stream the relay signs a short-lived (≤15 min, refreshed while the stream is open) EdDSA/Ed25519 token bound to the site, the resource, the grant and that one stream. The connector verifies it independently against the relay public key pinned at install time (delivered with the site token, not trust-on-first-use) and closes the stream with `invalid_grant` otherwise. This protects against relay *bugs*, not a relay *compromise*: the signing key (`TUNR_GRANT_SIGNING_KEY`) lives on the relay, so a compromised relay could mint grants. Approver-signed grants are future work.
- **Postgres proxy** (`internal/bridge/connector/pgproxy/`). Postgres resources are only served through a read-only proxy in the connector, in three layers: a `SELECT`-only login role (`tunr connect pg-role`); every session starts with `default_transaction_read_only=on` and a 30 s statement timeout; a statement classifier (allow-list over a SQL tokenizer) refuses writes, DDL, `COPY` and other unsafe statements with SQLSTATE `42501`. The role is the real guarantee; the classifier denies when unsure. The agent never sees the database password — its DSN carries a random per-process secret that only unlocks its own local port.
- **End-to-end TLS.** The connector holds a self-signed certificate and announces its SHA-256 fingerprint. `tunr access` runs a TLS 1.3 handshake inside the stream, pinned to that fingerprint (no CA), and the connector terminates it before its own checks and the Postgres proxy. The relay only forwards ciphertext, so anyone who can *read* the relay can't read the data. The relay is also what hands the fingerprint to the agent, so an actively compromised relay could substitute it; the fingerprint is shown in the portal and by `tunr connect status` for out-of-band checks. Streams to connectors that don't announce a certificate stay unencrypted end to end, and the agent logs it.
- **Grant sweeper.** A goroutine in `relay/internal/bridge/sweeper.go`, separate from the app idle sweeper, marks expired grants every few seconds, tells connectors to close any remaining streams (`bridge_kill`), audits and notifies. Streams also close on their own at `expires_at`. Revoking a grant, or "Cut all access" on a site, closes live streams on both sides; `tunr connect kill` does the same locally without the relay.
- **Audit.** Grant requests and decisions, every stream open/close (with byte counts) and a per-query summary from the connector (command, tables, rows, duration, blocked; query text only if the customer enables it) are written to `audit_log` with an `org_id`, and exportable as CSV. Every query is filtered by organization.
- **Notifications.** Grant events go by e-mail to the customer's approvers (Resend, optional) and to HMAC-signed outgoing webhooks (`X-Tunr-Signature: t=<unix>,v1=<hex>`). Delivery is asynchronous and best-effort; it never delays a decision.
- **Portal.** The relay embeds a static UI at `/portal/` (strict CSP, magic-link login) for customers and vendors, built on the same `/api/v1/bridge/*` API as the CLI.
- **Hosted agents (early access).** With `TUNR_BRIDGE_HOSTED_AGENTS=1`, `tunr agent deploy` builds and runs an agent through the same runner as `tunr deploy`. The runner mounts a static `tunr` CLI (`TUNR_AGENT_BINARY`) into the container; the agent gets a bootstrap secret (`TUNR_AGENT_BOOTSTRAP`) that it exchanges for 1-hour agent tokens. Hosted agents are not public: the relay only lets through HMAC-signed webhooks and in-process cron triggers (UTC). Egress and CPU quotas are not enforced yet, which is why they are off on tunr.sh.
- **Storage.** The bridge requires Postgres (`DATABASE_URL`); it has no in-memory mode. Schema: `relay/migrations/005`–`009`.

Further reading: [protocol/bridge.md](protocol/bridge.md) (wire protocol), [adr/0001-agent-bridge.md](adr/0001-agent-bridge.md) (design decision), [bridge/security.md](bridge/security.md) (guarantees and limits).

## 2. Local CLI Binary (The Core)
The heart of Tunr is a singular, statically compiled Go binary (`cmd/tunr`). It is completely cross-platform, natively compiled for Linux, macOS (Intel & Silicon), and Windows with CGO disabled. 
- **No Node.js/Python dependencies:** A pure Go client utilizing `Cobra` for CLI, and the native `net/http` package.
- **WebSocket Gateway:** Creates an encrypted tunnel with the Remote Relay using WebSockets and handles concurrent bidirectional HTTP transmission.

## 3. Advanced Local Proxy (The Middleware Hub)
Inside `internal/proxy`, Tunr hosts an intelligent `ReverseProxy`. It is not just a dumb pipe. It houses several custom middleware for the modern web:

### Real-time Inspector Ring Buffer (Faz 3)
A continuous `bytes.Buffer` acting as an interceptor ring. The last 1,000 requests are captured with exact body, headers (sanitized), and response times, readable locally through a streaming websocket embedded web-UI (`tunr open`).

### Vibecoder Proxy (Faz 8)
An arsenal of proxy-level manipulations specifically designed for safe, bullet-proof client demos.
1. **The Freeze Cache:** Implements a localized 5MB threshold `InMemory` Cache Layer. Every successful HTTP `200` GET request (html, scripts, assets) is securely hashed and stored. If local Next.js/Go server process gets killed or issues 5xx errors, the proxy silently routes from cache and marks the response with `X-Tunr-Freeze-Cache: 1`. 
2. **Read-Only / Mutate-Blocker:** Identifies incoming `POST`, `PUT`, `DELETE` operations using the `http.Method` attribute. Blocks database altering behavior. Synthesizes an identical artificial `200 Success` or `201 Created` HTTP Response with placeholder json values to satisfy frontend application states smoothly.
3. **HTML AST Injection:** The HTTP Response Writer is overridden (`http.ResponseWriter`). The compressed data (e.g., `gzip`) is automatically decoded via `compress/gzip` reader natively, parsed to find `</body>`, injected with `<script scr="...">` feedback/error UI modules, and cleanly re-encoded back to the network chunk. All transpiring under 12ms proxy latency.

## 4. The Relay Server
Tunr natively hosts an open-source Relay architecture (`relay/cmd/server/main.go`).
- **Connection Registry:** Stores active node pairs maintaining real-time subdomains mapped to respective client websocket IDs. Memory bounded and horizontally scalable.
- **Single region today:** tunr.sh runs one relay in the EU. `--region` is accepted and the balancer carries region metadata (`ams`, `sea`, `sin`), but multi-region routing is planned, not live.
- **Postgres Metadata:** Retains historical tunnel footprints, registered client keys, and webhook subscription plans managed by Paddle billing architecture (`tunr/internal/billing`).

## 5. Auth & Secrets
- `tunr` implements its own `auth.go` module utilizing standard OS-keychain methodologies to avoid laying plain-text `.env` tokens in workspace directories. 
- `tunr login` opens the dashboard, which hands a relay-format JWT (HS256, `TUNR_JWT_SECRET`) back to a loopback callback; the relay verifies it in `relay/internal/auth`. Anonymous tunnels work without login.
