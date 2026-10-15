# ADR-0001: Agent Bridge

- **Status:** Accepted (in progress on `bridge/main`)
- **Date:** 2026-10-14

## Context

AI vendors increasingly sell agents that need data living inside their
customers' networks: on-prem ERPs, ledgers, file servers, historians. Today each
vendor builds its own on-prem agent, asks for a site-to-site VPN, or copies the
data into its own cloud. All three over-grant access, are hard to audit, and
stall in security review.

tunr already has most of the transport this needs: an outbound WebSocket from
a process inside a private network to a public relay, with TCP streams
multiplexed over it (`tcp_open/tcp_data/tcp_close`), service installation
(`tunr service install`), and a gVisor runner that can host agents.

What it lacks: agent identity separate from human accounts, a customer-side
approval step, time-boxed grants, protocol-aware read-only enforcement, and a
per-query audit trail visible to the customer.

## Decision

Build a two-sided bridge on top of the existing relay:

- **Connector** (`tunr connect`) runs inside the customer network, dials the
  relay outbound only, and exposes *named* resources (`acme/erp-db`). Target
  addresses stay in the connector's local config and never reach the relay.
- **Agents** get their own short-lived tokens (`typ: "agent"`), distinct from
  human JWTs. Agent tokens are rejected on human endpoints and vice versa.
- **Grants** bind agent × resource × mode × TTL. Default deny, default
  read-only. The customer approves, revokes, or kills.
- For each stream the relay signs a short-lived **Ed25519 grant JWT** that the
  connector verifies against a relay key pinned at install time (fingerprint
  delivered with the site token, not trust-on-first-use).
- **Postgres** resources go only through a read-only proxy in the connector
  (`pgproto3`, pure Go): the agent never sees the database password; the
  connector logs in with a read-only role, forces
  `default_transaction_read_only=on`, and classifies statements as a second
  layer. The role is the real guarantee; the classifier denies when unsure.
- Everything is behind `TUNR_BRIDGE_ENABLED` on the relay. With it off the
  relay is unchanged.

## Alternatives considered

- **Mesh VPN per customer** (Tailscale/WireGuard style): grants network-level
  access to a whole subnet and has no customer-facing approval or per-query
  audit. Good for one organisation's own agents, poor across companies.
- **Identity-aware access proxies** (Teleport style): strong protocol-level
  control, but deployed and operated by the customer for their own users; the
  vendor-embedded, cross-company flow is not the primary model.
- **Copy data to the vendor's cloud:** loses freshness and customer control,
  and raises data-protection exposure.

## Consequences

- The CLI module gains a dependency on pgx (pure Go, no cgo): `pgproto3`
  for the wire protocol and `pgconn` to log in upstream (SCRAM, MD5, TLS)
  before hijacking the connection. This deliberately relaxes the previous
  "the CLI does not pull pgx" note. Cost: the CLI binary grows ~1.2 MB
  (10.9 → 12.2 MB, darwin/arm64).
- pgproxy does not forward cancel requests, and does not cancel upstream
  when a row limit trips: PostgreSQL's cancel can't name a query, so a
  cancel racing the end of one query kills the next. Over-limit results are
  drained instead, bounded by statement_timeout (30s).
- The connector lives in the CLI module, so it is Apache-2.0 like the rest of
  the CLI; relay-side bridge code is PolyForm Shield like the rest of the relay.
- New relay migrations add `orgs`, `links`, `sites`, `resources`, `agents`,
  `agent_tokens`, `grants`, `access_sessions`, `access_queries`, and an
  `org_id` column on the existing `audit_log`. No changes to `users`; personal
  orgs are created lazily on first bridge use.
- Connector config is JSON (`connector.json`, mode 0600, unknown fields
  rejected), consistent with `.tunr.json`; it holds no secrets. The site token
  and database credentials live in a secret store referenced by name.
- Secret store: the OS keychain by default. A headless Linux server running
  the connector as a systemd service has no keychain session, so a `file`
  store exists — one 0600 file per secret in a 0700 directory, refusing to
  read anything group/world-accessible. It is **not encrypted**; protection is
  file ownership. It is used only when chosen explicitly, and `tunr connect
  install` on Linux requires it. This is a deliberate, documented exception to
  "tokens never touch disk".
- **Known limits, stated plainly:**
  - The grant signing key lives on the relay. Connector-side verification
    protects against relay *bugs*, not a relay *compromise*. Approver-side
    signing is a later phase.
  - Streams are end-to-end TLS 1.3 between `tunr access` and the connector
    when both support it (the default since P8): the connector holds a
    self-signed certificate, the relay passes its SHA-256 to the agent, the
    agent pins it. The relay carries ciphertext — protection against anyone
    who can *read* the relay. It is not protection against a relay that is
    actively compromised: such a relay could hand the agent a different
    fingerprint. The fingerprint is shown in the portal and by
    `tunr connect status` so it can be checked out of band. Streams to
    older connectors stay unencrypted end to end (the agent logs it;
    `--require-e2e` refuses them). Query summaries still reach the relay —
    they are produced by the connector, after TLS.
  - Stream payloads are base64-in-JSON like `tcp_data`; fine for query traffic,
    inefficient for bulk export. Binary framing is a later phase.
