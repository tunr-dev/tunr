#!/bin/bash
# Demo connector: what Acme's IT does once, then the connector itself.
# State (config, file secret store) lives in the /etc/tunr volume, so a
# restart reuses it instead of setting the site up again.
set -euo pipefail
cfg=/etc/tunr/connector.json

if [ ! -f /etc/tunr/.ready ]; then
  echo "$SITE_TOKEN" | tunr connect --config "$cfg" --relay "$RELAY_URL" --site "$SITE_NAME" \
    --token-stdin --relay-key "$RELAY_KEY" --secret-store file --setup-only
  tunr connect resource add erp-db --config "$cfg" --kind postgres --target acme-db:5432 --database logo

  # The DBA's part: a SELECT-only role on the tables the vendor may read.
  # pg-role stores the generated password as erp-db's credential.
  for _ in $(seq 60); do pg_isready -q -h acme-db -U postgres && break; sleep 1; done
  tunr connect pg-role --config "$cfg" --database logo --tables cari,fatura,tahsilat,stok \
    --resource erp-db > /tmp/pg-role.sql
  PGPASSWORD="$ACME_DB_OWNER_PASSWORD" psql -q -v ON_ERROR_STOP=1 -h acme-db -U postgres -f /tmp/pg-role.sql
  rm -f /tmp/pg-role.sql
  touch /etc/tunr/.ready
fi

exec tunr connect --config "$cfg" --health-addr 0.0.0.0:9095
