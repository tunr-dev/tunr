# tunr Bridge — security overview

For the IT and security team of a company asked to connect an AI vendor's agent
to internal systems. It describes what the bridge guarantees, how, where each
guarantee is tested, and — just as plainly — what it does not guarantee.

## How it fits together

```
 vendor's agent ──► tunr access ──(TLS 1.3, pinned)──────────────────► connector ──► your database
 (anywhere)        127.0.0.1:port      ▲ relay forwards ciphertext ▲     (your network,      (read-only role)
                                       └──────── relay.tunr ───────┘     outbound only)
```

- The **connector** (`tunr connect`) runs on a machine you choose inside your
  network. It only makes an outbound HTTPS/WebSocket connection to the relay.
  No inbound port is opened.
- Each **resource** you expose has a name (`acme/erp-db`). Its address stays
  in the connector's local config; the relay and the vendor never learn it.
- A vendor's **agent** has its own identity, separate from any person.
- Nothing flows until one of your approvers approves a **grant**: this agent,
  this resource, this mode, this long, for this stated reason.

## Guarantees and where they are tested

| # | Guarantee | How | Tested by |
| --- | --- | --- | --- |
| 1 | **Default deny.** No grant, no stream. | The relay checks an approved, unexpired grant, an active agent and token, an active vendor link and an online connector before opening anything. | `relay/internal/bridge/access_test.go` `TestAccessRefusals`; e2e step 4 |
| 2 | **Databases are read-only.** | **PostgreSQL**, three layers: a login role with `SELECT` only (`tunr connect pg-role`); every session starts with `default_transaction_read_only=on` and a 30 s statement timeout; a statement classifier (allow-list over a real SQL tokenizer) refuses writes, DDL, `COPY`, function-call messages, row locks, `SET` of unsafe parameters and side-effect functions. Refusals return SQLSTATE `42501`. **SQL Server** (Nebim, Logo, Netsis…), three layers: a login with `SELECT` only and `DENY INSERT, UPDATE, DELETE, EXECUTE, ALTER` on the database (`tunr connect mssql-role`); every statement runs in a transaction the connector always rolls back, with a 5 s lock timeout so it can't block the ERP; a T-SQL classifier that allows only `SELECT`/`WITH` and refuses any write or code keyword anywhere in the batch (T-SQL needs no `;` between statements), `SELECT INTO`, `EXEC`, `OPENROWSET`/`OPENQUERY`, system procedures and cross-database or linked-server names. `tunr connect resource verify` logs in as the connector will and reports any right the login should not have. | `pgproxy` `TestClassify`, `TestProxyReadOnly` (real PostgreSQL); `mssqlq` `TestClassifyDenies`, `TestServeBlocksWrites`, `TestServeAuthAndQuery` (rollback); e2e step 7 |
| 3 | **The connector checks every stream itself.** | Each stream carries a short-lived (≤15 min) Ed25519-signed grant token bound to your site, the resource and that one stream, single use. The connector verifies it against the relay key pinned at install. | `internal/bridge/connector/streams_test.go`, `TestVerifyGrant` (tampering, wrong key, algorithm confusion, expiry, replay) |
| 4 | **The agent never sees your database password.** | The connector logs in with the read-only role from its own secret store, for PostgreSQL and SQL Server alike. The agent's local DSN (PostgreSQL) or query token (SQL Server) is a random per-process secret that only unlocks its own local port. | `TestProxyAuth`, `TestServeRefusesWrongSecret`, `TestAgentSessionAgainstConnector`; e2e step 7 |
| 5 | **No inbound ports.** | The connector dials out; the relay cannot dial in. | e2e step 8 (relay and agent cannot reach the customer network) |
| 6 | **Short-lived credentials.** | Grant tokens ≤15 min; agent tokens ≤24 h (hosted agents: 1 h, renewed); site tokens can be rotated from the portal at any time. | `relay/internal/auth` tests |
| 7 | **Everything is recorded.** | Grant requests, approvals, denials, revocations, expiries, every stream opened and closed (with byte counts) and a summary of every database query (command, tables, rows, duration, blocked) go to your audit log, exportable as CSV/JSON. Query *text* is kept only if you turn it on. | `TestAccessStreamRoundTrip`, `TestAccessQuerySummariesPersisted`; e2e step 9 |
| 8 | **You can cut access instantly.** | Revoking a grant closes its open connections on both sides. Revoking a vendor link, disabling a resource or an agent, or revoking an agent token also ends the open connections and grants they cover. “Cut all access” on a site revokes every approved grant, denies pending ones and turns auto-approve off, so agents can't reconnect without a person. A local `tunr connect kill` works even without the relay. | `TestRevokeCutsLiveStreamFast` (2 s bound; measured ~4 ms in e2e), `TestKillSiteRevokesAndCuts`, `TestRevocationCascades`, `TestKillSiteTurnsOffAutoApprove`, connector kill-switch test |
| 9 | **Tenants are isolated.** | Every database query is filtered by organization; another org's objects are indistinguishable from missing ones. | `TestBridgeTenantIsolation`, `TestAPITenantIsolation` (every endpoint) |
| 10 | **The relay can't read your data in transit.** | Streams are end-to-end TLS 1.3 between the vendor's `tunr access` and your connector, pinned to your connector's certificate. | `internal/bridge/access` `TestEndToEndTLSPinning`; e2e step 7 |
| 11 | **An agent is never more powerful than the person it acts for.** | Each grant records the vendor-side person behind it (who requested it, or who issued the agent's token) and you see "acting for …" before approving. When that person leaves the vendor's organization, their grants stop working immediately and their tokens can't ask for new ones. | `relay/internal/bridge` `TestGrantActsOnBehalfOfIssuer` |
| 12 | **Sensitive columns can be masked.** | PostgreSQL: the connector masks listed columns in results (`resource add --mask cari.vergi_no=partial:4`; `redact`, `partial`, `hash` with a site-local key, `null`). It follows the column through aliases (by table and column number), refuses expressions, aggregates and `UNION`s that touch a masked column, and accepts only the simple query protocol on a masked resource. SQL Server: SQL Server's own Dynamic Data Masking, set up by `mssql-role --mask`; the connector's login is `DENY UNMASK`. | `pgproxy` `TestProxyMasking` (real PostgreSQL) |

Read-write access exists only for raw TCP resources (a file server, a
protocol the bridge doesn't understand). Raw TCP cannot be made read-only, so
such grants always need **two different approvers**.

## What the bridge does not protect against

- **Guessing a masked value with WHERE.** Masking hides values in results. An
  agent that may read a table can still test a guess
  (`… WHERE tc_kimlik = '1234…'` returns the row or not); Microsoft documents
  the same limit for SQL Server's masking. For values an agent must never
  learn, leave the column or table out of the database role
  (`pg-role --tables`, `mssql-role --tables`).
- **Schema profiles are maps, not guarantees.** The Nebim, Logo and Netsis
  profiles come from public sources and vary between versions. They describe
  tables to the agent; they grant nothing. `resource verify` checks which
  tables exist; an unverified profile is always labelled as such.

- **An actively compromised relay.** End-to-end TLS stops anyone who can
  *read* the relay, but the relay tells the vendor which certificate to
  expect. A relay under an attacker's control could substitute its own. The
  relay also signs grant tokens. To check out of band, give vendors your
  connector's certificate fingerprint (portal → Sites, or
  `tunr connect status`). Self-hosting the relay removes the third party.
- **A compromised connector machine.** It holds the database credential and
  can reach the targets you configured. Run it on a machine you maintain like
  any other server; it only needs outbound HTTPS.
- **What a read-only agent does with data it was allowed to read.** The grant
  limits what, how much (`max_rows`, `max_bytes`, `qps`) and for how long; it
  cannot control what the vendor does with the result. That is a contract
  matter — the audit log is your evidence.
- **Query text in transit to the relay.** Your connector sends a summary of
  every query to the relay, including its (truncated) text, so they appear in
  your audit log. The relay stores the text only if you enabled
  `log_query_text`; otherwise it drops it and keeps the summary.

## Operational notes

- Connector secrets live in the OS keychain, or — on a headless server — in
  `0600` files in a `0700` directory (`--secret-store file`); the connector
  refuses to read them if other users can. They are not encrypted at rest:
  protection is file ownership.
- The connector's systemd unit runs with `ProtectSystem=strict`,
  `NoNewPrivileges`, private `/tmp` and devices, and outbound sockets only.
- On the vendor side, `tunr access` listens on `127.0.0.1` only. Postgres
  ports need the per-process DSN secret. Raw TCP has no password of its own:
  on Linux the port refuses connections from other users' processes; on
  macOS and Windows any local user can reach it while it is open, and
  `tunr access` says so.
- The site token is entered at a prompt (`--token-stdin`) so it stays out of
  shell history and process lists.
- Report vulnerabilities as described in [SECURITY.md](../../SECURITY.md).
