# tunr Bridge — quick start for the vendor (agent builder)

Your agent needs data inside a customer's network. With tunr Bridge the
customer installs one connector and approves your agent; your agent gets a
local port to the resource, never a password. Writes to databases are refused.

## 1. Organization and agent

Sign in to the dashboard (<https://app.tunr.sh>; on a self-hosted relay, the
portal at `https://<relay>/portal/`) and create an organization of
type **Vendor** (slug e.g. `finbot`).

Then send the customer one link. It walks their IT through creating their
organization and site and shows the connector install command; accepting it
links your org, nothing more — they approve every grant:

```bash
tunr invite create --org finbot --customer "Kuzucular Yapı" --note "Nebim stock for the sales assistant"
# https://app.tunr.sh/connect/tunr_inv_…   (one use, 7 days)
```

(Or tell them your slug and they allow you under **Vendors**.) Point their IT
team at [for-your-it-team.md](for-your-it-team.md).

Create an agent identity per agent or product feature. Customers see its
name, and the person who issued its token: grants act for that person and stop
working if they leave your org.

```bash
tunr login
tunr agent create invoice-reconciler --org finbot
export TUNR_AGENT_TOKEN=$(tunr agent token invoice-reconciler --org finbot --ttl 24h)
export TUNR_RELAY_URL=https://bridge.tunr.sh   # or your own relay
```

## 2. Find and request access

```bash
tunr catalog
tunr access request acme/erp-db --ttl 2h --reason "Reconcile October invoices" --wait 15m
```

Write the reason for the customer's approver: what you will read and why.
Databases are read-only; raw TCP resources need read-write, which the customer
must approve with two people.

## 3. Use it

```bash
tunr access acme/erp-db --port 15432
# prints: postgresql://agent:<local secret>@127.0.0.1:15432/db?sslmode=disable
psql "postgresql://agent:…@127.0.0.1:15432/db?sslmode=disable" -c "select count(*) from invoices"
```

- The stream is end-to-end TLS to the customer's connector, pinned to its
  certificate; `--require-e2e` refuses connectors too old for it. Compare the
  fingerprint the customer gave you with what `tunr access` reports.
- Writes fail with SQLSTATE `42501`. Grants can limit rows per query, bytes
  and queries per second; respect them in your queries (`LIMIT`, `WHERE`).
- Access stops when the grant expires or the customer revokes it — handle a
  closed connection as "ask again", not as an error to retry blindly.

**SQL Server resources** (`kind: mssql`) are served in query mode: the
customer's connector runs each statement with its own read-only login.

```bash
tunr access acme/nebim --port 15433
# TUNR_QUERY_URL=http://127.0.0.1:15433/query
# TUNR_QUERY_TOKEN=…
curl -s "$TUNR_QUERY_URL" -H "Authorization: Bearer $TUNR_QUERY_TOKEN" \
  -d '{"sql":"SELECT TOP 5 ItemCode, AvailableInventoryQty1 FROM ItemInventory"}'
```

T-SQL, `SELECT`/`WITH` only: no `SET`, `EXEC`, temp tables or cross-database
names. Responses are JSON (`result_sets`, `row_count`, `truncated`).

**Know the schema before writing SQL.** `tunr catalog` shows each resource's
profile (`nebim-v3`, `logo-tiger`, `netsis`); `tunr profile show logo-tiger`
lists tables, rules (Logo's `CANCELLED = 0`, `ITEMS.ACTIVE` 0 = active) and
example queries. A profile the customer hasn't verified is marked so.

## From an agent framework (MCP)

Point your MCP client at `tunr mcp` with `TUNR_AGENT_TOKEN` in its
environment. The agent gets `tunr_list_resources`, `tunr_describe_resource`
(the schema profile, verified or not), `tunr_request_access`,
`tunr_access_status`, `tunr_query` (one read-only statement, rows as JSON,
200 rows by default), `tunr_open_access` (returns the DSN or query endpoint)
and `tunr_close_access`. For your coding agent, paste
[agent-prompt.md](agent-prompt.md).

```json
{"mcpServers": {"tunr": {"command": "tunr", "args": ["mcp"],
  "env": {"TUNR_AGENT_TOKEN": "…", "TUNR_RELAY_URL": "https://<relay>"}}}}
```

## Run the agent on tunr (optional, early access)

Hosted agents are off on tunr.sh until egress and CPU quotas are enforced;
ask for access, or enable them on your own relay with
`TUNR_BRIDGE_HOSTED_AGENTS=1` and a runner started with `TUNR_AGENT_BINARY`.

```bash
tunr agent deploy invoice-reconciler --org finbot --volume 256 ./agent
tunr agent trigger add invoice-reconciler --org finbot --cron "0 7 * * *"
tunr agent trigger add invoice-reconciler --org finbot --webhook new-invoice --path /hooks/invoice
```

A hosted agent listens on `$PORT` and receives `POST` on its trigger path
(default `/run`), signed in `X-Tunr-Trigger-Signature` with
`TUNR_TRIGGER_SECRET`: `t=<unix>,v1=<hex HMAC-SHA256(secret, "<unix>.<body>")>`.
Keep the request open while it works — the app is not paused mid-request. Its
identity is automatic: the bundled CLI at `/opt/tunr/bin/tunr` (and `tunr mcp`)
exchange `TUNR_AGENT_BOOTSTRAP` for hourly tokens. It is not reachable from the
internet except through signed webhooks; `/data` persists across restarts.

Security details and limits: [security.md](security.md).
