# tunr Bridge — quick start for the customer (data owner)

You keep data inside your network and an AI vendor wants their agent to read
some of it. With tunr Bridge you run one connector, decide which systems it
may reach, and approve every agent, resource and time window yourself. Plan
on 10–15 minutes.

**Got a link from your vendor?** Open it, sign in, name your organization and
your first site, and the page shows the install command — steps 1, 2 and 4
in one go. Accepting the link approves nothing: you still decide on every
request (step 5).

## 1. Sign in and create your organization

Open the dashboard (<https://app.tunr.sh>; on a self-hosted relay, the portal
at `https://<relay>/portal/`), sign in with your work e-mail
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
  --relay-key ed25519:… --token-stdin --secret-store file --setup-only
# paste the site token (tunr_site_…) at the prompt
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

**SQL Server (Nebim, Logo, Netsis and most Turkish ERPs).** Add it as an
`mssql` resource. The connector runs each statement itself with a read-only
login and returns the rows — the agent never gets a SQL Server login:

```bash
sudo tunr connect resource add nebim --kind mssql --target 10.0.0.7:1433 --database NEBIM_V3 --profile nebim-v3
sudo tunr connect mssql-role --database NEBIM_V3 --tables cdItem,cdItemDesc,ItemInventory --resource nebim > mssql-role.sql
sqlcmd -S 10.0.0.7 -U sa -i mssql-role.sql      # your DBA runs this once
```

Without `--tables` the login can read every table (`db_datareader`); writes,
stored procedures and schema changes are denied either way.

**Check it.** `verify` logs in exactly as the connector will, tells you if the
login could write, and matches the schema profile against your database:

```bash
sudo tunr connect resource verify nebim
```

Profiles (`nebim-v3`, `logo-tiger`, `netsis`; `tunr profile list`) tell the
vendor's agent what the tables mean so it doesn't guess. Only the matched
table *names* are shared, and only after `verify`.

**Hide sensitive columns.** For PostgreSQL, mask columns in results:
`--mask cari.vergi_no=partial:4` (also `redact`, `hash`, `null`) on
`resource add`. For SQL Server, `mssql-role --mask
dbo.cdCurrAcc.IdentityNum='partial(0,"*******",4)'` sets up SQL Server's own
Dynamic Data Masking — read the warning it prints: masking applies to every
login without `UNMASK`. Masking hides values but can't stop an agent testing a
guess with `WHERE`; leave truly secret columns out of the role.

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

Requests appear under **Approvals** with the agent, the person at the vendor
it acts for, the resource, read-only or read-write, the duration and the
vendor's reason. Approve or deny. Access ends by itself when the time is up,
and as soon as that person leaves the vendor's organization.

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
