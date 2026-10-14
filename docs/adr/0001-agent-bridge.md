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

- The CLI module gains a dependency on `github.com/jackc/pgx/v5/pgproto3`
  (pure Go, no cgo). This deliberately relaxes the previous "the CLI does not
  pull pgx" note for the wire-protocol package only.
- The connector lives in the CLI module, so it is Apache-2.0 like the rest of
  the CLI; relay-side bridge code is PolyForm Shield like the rest of the relay.
- New relay migrations add `orgs`, `links`, `sites`, `resources`, `agents`,
  `agent_tokens`, `grants`, `access_sessions`, `access_queries`, and an
  `org_id` column on the existing `audit_log`. No changes to `users`; personal
  orgs are created lazily on first bridge use.
- **Known limits, stated plainly:**
  - The grant signing key lives on the relay. Connector-side verification
    protects against relay *bugs*, not a relay *compromise*. Approver-side
    signing is a later phase.
  - Until end-to-end TLS between `tunr access` and the connector ships, the
    relay can see stream bytes. (`tunr tls` today is not SNI passthrough; this
    is new work.)
  - Stream payloads are base64-in-JSON like `tcp_data`; fine for query traffic,
    inefficient for bulk export. Binary framing is a later phase.
