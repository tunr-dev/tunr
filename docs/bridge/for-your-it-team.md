# For your IT team: why your AI vendor uses tunr Bridge

Türkçe: [bt-ekibiniz-icin.md](bt-ekibiniz-icin.md)

An AI vendor wants their agent to read data from a system inside your
network — an ERP, an accounting database, a file server. They asked you to run
**tunr Bridge**. This page answers what your IT and security team will ask
before saying yes. Technical detail and tests: [security.md](security.md).

## What it is, in one paragraph

You run one small program, the **connector**, on a machine inside your
network. It connects *out* to the tunr relay over HTTPS — you open no inbound
port and no VPN. You tell it which systems it may reach. The vendor's agent
can then *ask* for access to one of them, for a stated reason and a limited
time. Nothing flows until a person on your side approves. You see every
request, every connection and every query, and you can cut access in seconds.

## Before you install: get these answers from the vendor

| Ask | Why |
| --- | --- |
| Which system, which tables, read or write? | Name each resource yourself; the vendor sees only the names you give. |
| Which agent and which person on their side is behind it? | Every grant shows the agent and the person it acts for; it stops if that person leaves the vendor. |
| How long do they need, how often? | Grants expire on their own (default ceiling 8 h, yours to set). |
| What do they do with the data afterwards? | The bridge limits what and how much is read, not what is done with it — that is a contract question. |
| Who do you call when something looks wrong? | Agree on a contact; you can cut access first and ask later. |

**Then test it together:** ask the vendor to show you one query that works
with an approved grant, one that is refused without a grant, and one write
that is refused even with one. All three appear in your activity log.

## How it works

```
 vendor's agent ──► tunr access ──(TLS 1.3, pinned to your connector)──► connector ──► your database
 (their cloud)                    the relay forwards ciphertext only      (your network,   (read-only login)
                                                                           outbound only)
```

- **Outbound only.** The connector dials the relay on port 443. Allow egress
  to the relay host; nothing needs to reach your network.
- **Addresses stay with you.** The database address and its password stay in
  the connector's local config and secret store. The relay and the vendor see
  a name like `acme/erp-db`.
- **The agent never gets a database password.** The connector logs in with a
  read-only login you create (`tunr connect pg-role` / `mssql-role` print the
  SQL for your DBA) and runs or filters every statement itself.
- **Read-only, three times over.** The login can only `SELECT`; every session
  is read-only (Postgres) or rolled back (SQL Server); a statement filter
  refuses anything that writes. `tunr connect resource verify` checks the login
  and tells you if it could write.
- **Sensitive columns** can be masked in results (Postgres) or with SQL
  Server's own Dynamic Data Masking. Masking hides values; it doesn't stop a
  determined agent testing a guess with `WHERE`, so leave truly secret columns
  out of the login entirely.

## What the relay (tunr) can and can't see

- It **can't read** your data in transit: streams are end-to-end encrypted
  between the vendor's machine and your connector.
- It **does see** who asked for what and when, and a summary of each query
  (command, tables, row count, duration) — that is your audit log. Query
  *text* is stored only if you turn it on.
- An **actively compromised relay** could try to impersonate your connector to
  the vendor. Give the vendor your connector's certificate fingerprint
  (`tunr connect status`) through a channel you trust; self-hosting the relay
  removes the third party.

## Stopping access

| How fast | What |
| --- | --- |
| One grant | **Revoke** in the portal — open connections close (measured ~4 ms). |
| One site | **Cut all access** — every grant on the site is revoked, pending ones denied, auto-approve turned off. |
| Without the relay | `sudo tunr connect kill` on the connector machine — stays stopped across restarts until `tunr connect resume`. |
| Permanently | `sudo tunr connect uninstall`, then block the relay host at the firewall. Revoke the vendor link in the portal. |

## Records you keep

**Activity** shows every session and every query summary; blocked statements
are highlighted. Export the audit log as CSV or JSON for your records or your
KVKK / GDPR file. A query summary records what the agent ran, not what the
vendor did with the result.

## Common questions

**Why not a VPN or an open port?** Both give the vendor a network path into
your LAN. The connector gives them one named system, read-only, for a limited
time, and only after you approve.

**What does the connector need?** A machine that can reach the database and
make outbound HTTPS — a Linux server is the usual choice, where it runs as a
hardened systemd service; a Mac works too. It is a single static binary.

**Can the vendor approve their own access?** No. Approvals come from people in
your organization with the approver role; read-write access to raw TCP
resources needs two different approvers. Auto-approval exists only if you turn
it on for one vendor, read-only, within a time limit you set.

**Is it open source?** The CLI and connector are Apache-2.0
([github.com/tunr-dev/tunr](https://github.com/tunr-dev/tunr)); the relay is
source-available (PolyForm Shield) and can be self-hosted.
