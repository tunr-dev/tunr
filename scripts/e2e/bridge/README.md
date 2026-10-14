# Agent Bridge end-to-end test

```bash
./scripts/e2e/bridge/run.sh          # build, run, assert, tear down
KEEP=1 ./scripts/e2e/bridge/run.sh   # keep the stack up to poke at it
```

Needs Docker (compose v2), `curl`, `jq` and `python3`.

The stack separates three networks so the test proves the bridge is the only
path to customer data:

| Network | Members | Notes |
| --- | --- | --- |
| `relay-net` | relay, relay-db, connector, agent | stands in for the internet |
| `customer-net` | customer-pg, customer-echo, connector | `internal: true` |
| `vendor-net` | agent | `internal: true` |

What it checks (P3 scope): connector comes online outbound-only and no target
address reaches the relay database; access without a grant is refused; an
`ro` grant on a raw TCP resource is refused by the connector; an `rw` grant
carries bytes and a real `psql` query; the agent and the relay cannot reach the
customer network directly; sessions and audit events are recorded.

Grants are approved with SQL until the approval API exists (P4). Postgres
resources go through the read-only proxy from P5 on; until then the test uses
a raw `tcp` resource for the database.
