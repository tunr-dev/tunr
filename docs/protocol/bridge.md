# Agent Bridge wire protocol

Status: draft. Implemented on `bridge/main`; relay side only active with
`TUNR_BRIDGE_ENABLED=1`.

The bridge reuses the tunnel framing: one WebSocket, JSON messages with a
`type` discriminator. Bridge messages use their own endpoints, so they never
appear on `/tunnel/connect`; existing message types are unchanged. Both peers
must ignore unknown `type` values (forward compatibility).

## Endpoints

| Endpoint | Peer | Auth |
| --- | --- | --- |
| `GET /bridge/connect` (WS) | connector, one per site | site token |
| `GET /bridge/access` (WS) | agent (`tunr access`), one per local TCP connection | agent token |
| `/api/v1/bridge/*` (HTTP) | humans (portal, CLI) and agents (grant requests) | human JWT or agent token, per route |

All authentication happens **before** the WebSocket upgrade; a bad token gets a
plain HTTP 401 and no upgrade.

## Connector ↔ relay (`/bridge/connect`)

| Type | Direction | Fields | Meaning |
| --- | --- | --- | --- |
| `bridge_hello` | C→R | `site_token`, `connector_version`, `resources[] {name, kind}` | Announce site and resource *names* (never addresses) |
| `bridge_welcome` | R→C | `site_id`, `relay_pubkey`, `resources[] {name, id}` | Connector compares `relay_pubkey` with the fingerprint from its install command and disconnects on mismatch |
| `bridge_open` | R→C | `stream_id`, `resource_id`, `grant_jwt` | Open an authorised stream |
| `bridge_data` | both | `stream_id`, `payload` (base64) | Same encoding as `tcp_data` |
| `bridge_close` | both | `stream_id`, `reason` | `client`, `expired`, `revoked`, `killed`, `invalid_grant`, `error` |
| `bridge_audit` | C→R | `stream_id`, `command`, `tables[]`, `row_count`, `duration_ms`, `blocked`, `query_text?` | Per-query summary; `query_text` only if the customer org enabled it |
| `bridge_kill` | R→C | `grant_id` or `"*"` | Close every stream for a grant (or all) now |

## Agent ↔ relay (`/bridge/access`)

1. Agent → `access_hello {resource: "<org-slug>/<name>", grant_id?}`
2. Relay → `access_ready {stream_id, grant_expires_at}` or
   `access_error {code}` where `code` ∈ `no_grant`, `grant_pending`,
   `site_offline`, `unknown_resource`, `forbidden`.
3. Then `bridge_data` / `bridge_close` as above.

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
