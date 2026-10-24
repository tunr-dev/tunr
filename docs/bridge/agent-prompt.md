# tunr Bridge — brief for your coding agent

Paste everything below the line into Claude Code, Cursor or any coding agent
that is wiring your product to tunr Bridge. Every value in it was checked
against tunr v0.9 (`cmd/tunr`, `internal/mcp`, `relay/internal/bridge`).

---

You are integrating with **tunr Bridge**, which lets our agent read a
customer's on-premises database through a connector the customer runs. Facts
and rules:

**Identity.** Our agent authenticates with `TUNR_AGENT_TOKEN` (create with
`tunr agent token <agent> --org <vendor-org> --ttl 24h`; ≤24 h). Set
`TUNR_RELAY_URL` to the relay (e.g. `https://bridge.tunr.sh`). Never log the
token. Grants act for the person who issued the token; if they leave our org,
grants stop and new requests fail with `issuer_left` — issue a new token.

**Addresses.** Resources are `<customer-slug>/<resource-name>`, e.g.
`acme/nebim`. Kinds: `postgres` and `mssql` (always read-only), `tcp` (raw
bytes, read-write, needs two customer approvers).

**Flow (MCP tools from `tunr mcp`):**
1. `tunr_list_resources` — what the customer shares and our grant status.
2. `tunr_describe_resource` — the schema profile (Nebim V3, Logo Tiger,
   Netsis…): tables, column meanings, rules, example queries. Check
   `verified`: an unverified profile's names may not match this customer's
   database — confirm with `INFORMATION_SCHEMA.TABLES` / `COLUMNS` first.
3. `tunr_request_access` with a `reason` (required, ≤500 chars, the
   customer's approver reads it: say what you will read and why) and a `ttl`
   between `1m` and the customer's maximum (default `8h`; we default to `1h`).
   Request the shortest time the task needs.
4. Poll `tunr_access_status` (status `pending` → `approved` | `denied`). A
   person decides; tell the user you are waiting instead of polling fast.
5. `tunr_query` with one statement. Results are capped at 200 rows unless you
   pass `max_rows`; the grant's own limit still applies. Prefer `COUNT`, `SUM`,
   `GROUP BY` and `TOP`/`LIMIT` to pulling rows.
6. `tunr_close_access` when done.

Without MCP: `tunr access request <addr> --reason "…" --ttl 1h --wait 10m`,
then `tunr access <addr> --port N`. Postgres prints a `postgresql://` DSN;
mssql prints `TUNR_QUERY_URL` and `TUNR_QUERY_TOKEN` for
`POST /query {"sql": "…", "max_rows": 100}` with
`Authorization: Bearer $TUNR_QUERY_TOKEN`.

**SQL rules that cost a day if missed:**
- Writes are refused: SQLSTATE `42501` on Postgres, error code `blocked` on
  mssql. Don't retry a refused statement; change it.
- mssql is T-SQL, `SELECT` or `WITH` only. Refused anywhere in the batch:
  `INSERT/UPDATE/DELETE/MERGE`, `SELECT … INTO`, `EXEC`, `SET`, `DECLARE`,
  transactions, `WAITFOR`, `OPENROWSET`/`OPENQUERY`, `xp_`/`sp_` procedures,
  `NEXT VALUE FOR`, and three-part names (`otherdb.dbo.t`, `db..t`). A column
  named like a keyword must be bracketed: `[Update]`.
- Use `WITH (NOLOCK)` on big ERP transaction tables; the connector also gives
  up on locks after 5 s. Statements time out after 30 s.
- mssql values: `decimal`/`money` arrive as strings, `uniqueidentifier` as
  text, binary as `0x…`, dates as RFC 3339.
- Postgres resources with masked columns accept only the simple query
  protocol (psql, psycopg2, `tunr_query`; pgx needs
  `default_query_exec_mode=simple_protocol`), show masked values as
  `****`, `******1234` or `h:…`, and refuse expressions over a masked column.
- Logo: tables are `LG_<firm>_<table>` and `LG_<firm>_<period>_<table>`;
  find the numbers first. Always `CANCELLED = 0`; `ITEMS.ACTIVE` 0 = active.
- Limits: row limit → `truncated: true` (mssql) or SQLSTATE `54000`
  (Postgres); byte budget → `data_limit` and the session ends; query rate →
  `rate_limited` / `53400`.

**Errors from `tunr access` / `tunr_open_access`:** `no_grant` (ask first),
`site_offline` (the customer's connector is down: tell the user, don't loop),
`unknown_resource` (not linked or wrong address), `invalid_token`,
`too_many_streams` (>32 open streams per token). A stream closed with
`expired` or `revoked` means ask again — never retry blindly.

**Security posture to keep:** don't cache customer rows longer than the task
needs, don't put them in logs, and treat the customer's audit log as the
record of everything we ran — every statement is summarised for them.
