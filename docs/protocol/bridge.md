# Agent Bridge wire protocol

Status: draft. Implemented on `bridge/main`; relay side only active with
`TUNR_BRIDGE_ENABLED=1`.

The bridge reuses the tunnel framing: one WebSocket, JSON messages
`{"type": "...", "data": {...}}` (the same envelope as the tunnel protocol;
the field tables below describe `data`). Bridge messages use their own endpoints, so they never
appear on `/tunnel/connect`; existing message types are unchanged. Both peers
must ignore unknown `type` values (forward compatibility).

## Endpoints

| Endpoint | Peer | Auth |
| --- | --- | --- |
| `GET /bridge/connect` (WS) | connector, one per site | `Authorization: Bearer tunr_site_…`; requests with an `Origin` header are refused |
| `GET /bridge/access` (WS) | agent (`tunr access`), one per local TCP connection | agent token |
| `/api/v1/bridge/*` (HTTP) | humans (portal, CLI) and agents (grant requests) | human JWT or agent token, per route |

All authentication happens **before** the WebSocket upgrade; a bad token gets a
plain HTTP 401 and no upgrade.

## Connector ↔ relay (`/bridge/connect`)

| Type | Direction | Fields | Meaning |
| --- | --- | --- | --- |
| `bridge_hello` | C→R | `connector_version`, `resources[] {name, kind, profile?, profile_tables?, profile_verified_at?}`, `tls_cert_sha256?` | Announce resource *names* (never addresses). Names: 3–32 chars `[a-z0-9-]`; kinds `postgres`, `mssql`, `tcp`, `http` (`http` is reserved: announced but not grantable yet, and the CLI cannot create one); max 100. `profile` names a built-in schema profile (`[a-z0-9-]`, ≤41 chars); `profile_tables` (≤500 names) are the profile's tables `tunr connect resource verify` found in the real database, with `profile_verified_at` (RFC 3339). Relays before v0.9 refuse the `mssql` kind; they ignore the profile fields. The site's resources become exactly this set (missing ones go `offline`); a name served by another site of the same org is refused (`resource_conflict`) |
| `bridge_welcome` | R→C | `site_id`, `site_name`, `relay_key`, `resources[] {name, id}` | Connector compares `relay_key` with the key from its install command and disconnects on mismatch |
| `ping` / `pong` | R→C / C→R | — | Every 30 s; the relay drops a connector silent for 75 s |
| `error` | R→C | `code`, `message` | Sent before a relay-initiated close: `bad_hello`, `resource_conflict`, `replaced` (a newer connector for the same site connected), `disconnected` (token rotated, site deleted) |
| `bridge_open` | R→C | `stream_id`, `resource_id`, `grant_jwt`, `client_secret_sha256?`, `e2e?` | Open an authorised stream. `e2e`: the agent will TLS-handshake with the connector inside the stream |
| `bridge_data` | both | `stream_id`, `payload` (base64) | Same encoding as `tcp_data` |
| `bridge_close` | both | `stream_id`, `reason` | `client`, `expired`, `revoked`, `killed`, `invalid_grant`, `error` |
| `bridge_audit` | C→R | `stream_id`, `command`, `tables[]`, `row_count`, `duration_ms`, `blocked`, `query_text?` | Per-query summary; `query_text` only if the customer org enabled it |
| `bridge_kill` | R→C | `grant_id` or `"*"` | Close every stream for a grant (or all) now |

## Agent ↔ relay (`/bridge/access`)

`GET /bridge/access?resource=<customer-slug>/<name>[&grant=grt_…]` with
`Authorization: Bearer <agent token>`. One WebSocket per agent-side TCP
connection.

All checks run before the upgrade; a refusal is a plain HTTP response with
`{"error": code, "message": …}`:

| Status | `error` | Meaning |
| --- | --- | --- |
| 401 | `auth_required`, `invalid_token` | missing, invalid, expired or revoked agent token; disabled agent |
| 400 | `bad_resource` | address is not `<customer>/<name>` |
| 404 | `unknown_resource` | no such active resource, or the customer has not linked the vendor (indistinguishable) |
| 403 | `no_grant` | no approved, unexpired grant for this agent × resource |
| 503 | `site_offline` | the customer's connector is not attached |
| 429 | `too_many_streams` | more than 32 open streams for this agent token |

After the upgrade:

- **Binary messages** carry raw stream bytes, both directions.
- **Text messages** carry JSON control:
  - relay → agent, first: `access_ready {stream_id, grant_id, mode, grant_expires_at, e2e, site_cert_sha256?}`
  - either way: `access_close {stream_id, reason}` — reasons include `client`,
    `expired` (grant end; the relay closes the stream at `expires_at`),
    `revoked`, `site_offline`, `invalid_grant` (connector refused),
    `slow_consumer`.

The relay forwards bytes to the connector as `bridge_data` (base64, ≤32 KiB
per message). A connector's `bridge_data`/`bridge_close` are only accepted
for streams of its own site.

**End-to-end TLS.** The agent sends `X-Tunr-E2E: 1`. If the site's connector
announced `tls_cert_sha256`, the relay sets `e2e` on `bridge_open` and
`access_ready`; the agent then runs a TLS 1.3 client handshake inside the
stream, pinning `site_cert_sha256` (no CA), and the connector terminates it
with its certificate before its own access checks and pgproxy. The relay only
ever forwards ciphertext. A relay that is actively compromised could substitute
the fingerprint; vendors can verify it out of band (portal, `tunr connect
status`).

**TCP resources and modes.** Raw TCP cannot be made read-only, so the
connector opens `tcp` resources only for `rw` grants (which require two
approvers, P4). `ro` is enforced for `postgres` resources by the connector's
read-only proxy (P5) and for `mssql` resources by query mode (below).

## Query mode (`mssql`)

An `mssql` stream does not carry the TDS protocol. The connector logs in to
SQL Server itself with the read-only login from its secret store, runs each
statement inside a transaction it always rolls back, and answers in
newline-delimited JSON (`internal/bridge/query`). The agent speaks first:

```
→ {"auth": "<client secret>"}                    once; its SHA-256 came with bridge_open
← {"ok": true, "kind": "mssql"}                  or {"error": {code, message}} and close
→ {"id": "1", "sql": "SELECT …", "max_rows": 100}
← {"id": "1", "result_sets": [{"columns": [{name, type}], "rows": [[…]]}],
   "row_count": 3, "truncated": false, "notice": "…", "duration_ms": 12}
```

Requests are answered in order. Lines are capped at 256 KiB (request) and
8 MiB (response; larger results come back `truncated`). `max_rows` can only
narrow the grant's limit. Error codes: `auth_failed`, `upstream`, `blocked`
(the T-SQL filter refused the statement), `rate_limited`, `data_limit` (the
session's byte budget is spent; the connector closes the session),
`db_error`, `bad_request`. Values: numbers and booleans as JSON, `decimal` /
`money` as strings, `uniqueidentifier` as its canonical text, binary as
`0x…`, dates as RFC 3339 (`date` as `YYYY-MM-DD`).

`tunr access` exposes the session on `127.0.0.1` as HTTP: `POST /query` with
`{"sql": …}` and `Authorization: Bearer <client secret>`; MCP clients use
`tunr_query`.

## Grants act for a person

A grant records the vendor-side person it acts for: the human who requested
it, or the human who issued the agent token that did (`requested_by`; the
portal shows `on_behalf_of`). The relay only opens streams for a grant while
that person is still a member of the vendor org, and an agent whose token's
issuer left the org can't request new grants (`403 issuer_left`). Hosted
agents' bootstrap tokens have no issuer; their grants are unaffected.

## Connect invites

A vendor admin creates a one-time link: `POST /api/v1/bridge/orgs/{org}/invites`
`{customer_hint?, note?, ttl?}` → `{invite, token, url}` (`url` is
`<dashboard>/connect/<token>`, default lifetime 7 days, at most 30). Anyone
with the link may read `GET /api/v1/bridge/invites/{token}` — the vendor's
name, the note, expiry and status, nothing else. A signed-in customer accepts
with `POST /api/v1/bridge/invites/{token}/accept` and either `{org}` (an
existing customer org they administer) or `{org_name, org_slug}` (creates
one), plus an optional `{site_name}`. Accepting links the vendor and creates
the site (its token is returned once, as from `POST …/sites`); it approves no
grant. A second use, a revoked or an expired link answers `410`.

## Grant JWT

EdDSA (Ed25519), lifetime ≤ 15 minutes, refreshed by the relay for open
streams.

```json
{
  "iss": "<relay domain>",
  "sub": "agt_…",
  "aud": "site_…",
  "res": "res_…",
  "gid": "grt_…",
  "mode": "ro",
  "lim": {"max_rows": 10000, "qps": 5},
  "exp": 1760000000,
  "jti": "acs_…"
}
```

The connector rejects the stream (`bridge_close{reason:"invalid_grant"}`)
unless: signature verifies with the pinned key, `aud` is its own site ID,
`res` is a resource in its local config, and `exp` is in the future.

## Identifiers

| Prefix | Object |
| --- | --- |
| `org_` | organisation |
| `lnk_` | vendor ↔ customer link |
| `site_` | customer site / connector |
| `res_` | resource |
| `agt_` | agent |
| `atk_` | agent token (JWT `jti`) |
| `grt_` | grant |
| `acs_` | access session (stream) |
