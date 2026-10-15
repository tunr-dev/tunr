# tunr Bridge — quick start for the customer (data owner)

You keep data inside your network and an AI vendor wants their agent to read
some of it. With tunr Bridge you run one connector, decide which systems it
may reach, and approve every agent, resource and time window yourself. Plan
on 10–15 minutes.

## 1. Sign in and create your organization

Open the portal (`https://<relay>/portal/`), sign in with your work e-mail
and create an organization of type **Customer**. Its slug (e.g. `acme`) is
the first half of every resource address (`acme/erp-db`).

Add colleagues under **Members**. Give the people who decide on access the
**approver** role. Read-write access always needs two different approvers.

## 2. Add a site

**Sites → Add a site.** A site is one network location (an office, a data
centre). The portal shows the commands below once, with your token and the
relay's key filled in. On a Linux server inside that network:

```bash
curl -fsSL https://tunr.sh/install.sh | sh
sudo tunr connect --relay https://<relay> --site istanbul-office \
  --token tunr_site_… --relay-key ed25519:… --secret-store file --setup-only
```

The connector only makes outbound HTTPS connections. Open no firewall ports.

## 3. Name what agents may reach

```bash
sudo tunr connect resource add erp-db --kind postgres --target 10.0.0.5:5432 --database logo
```

The address stays on this machine — the relay and vendors only see
`acme/erp-db`. For a database, create a read-only login role and let the
connector keep its password:

```bash
sudo tunr connect pg-role --database logo --tables invoices,stock --resource erp-db > pg-role.sql
psql -h 10.0.0.5 -U postgres -f pg-role.sql      # your DBA runs this once
```

Then start it as a service:

```bash
sudo tunr connect install
sudo tunr connect status        # shows the site's TLS fingerprint
```

The site turns **online** in the portal. Optionally set limits per resource
(rows per query, queries per second) under **Sites → Edit**.

## 4. Allow a vendor

**Vendors → Allow a vendor**, with their organization slug. This only lets
them see your resource names and ask; nothing is granted yet. Give them the
connector's TLS fingerprint (from `tunr connect status` or the portal) through
a channel you trust, so they can check it.

## 5. Approve requests

Requests appear under **Approvals** with the agent, the resource, read-only or
read-write, the duration and the vendor's reason. Approve or deny. Access ends
by itself when the time is up.

- **Revoke** closes that agent's open connections immediately.
- **Sites → Cut all access** revokes every approved grant on the site and
  closes every connection.
- On the connector machine, `sudo tunr connect kill` stops everything locally,
  even without the relay; `tunr connect resume` undoes it.

## 6. Watch and export

**Activity** shows every session and every query summary (command, tables,
rows, time); blocked statements are highlighted. **Export audit log (CSV)**
for your records. To keep full query text, turn it on under **Settings**.

## Monitoring

Run the connector with `--health-addr 127.0.0.1:9095` and point your
monitoring at `GET /healthz` (200 when connected, 503 when not).

Security details and limits: [security.md](security.md).
