#!/bin/bash
# Connector container entrypoint: first-time setup from env, resources, run.
set -euo pipefail
cfg=/etc/tunr/connector.json
echo "$SITE_TOKEN" | tunr connect --config "$cfg" --relay "$RELAY_URL" --site "$SITE_NAME" \
  --token-stdin --relay-key "$RELAY_KEY" --secret-store file --setup-only
tunr connect resource add nas    --config "$cfg" --kind tcp --target customer-echo:7777
tunr connect resource add erp-db --config "$cfg" --kind postgres --target customer-pg:5432 --database logo

# What the customer's DBA does once: create the read-only role. pg-role also
# stores the generated password as erp-db's credential, so nobody copies it.
for _ in $(seq 30); do pg_isready -q -h customer-pg -U postgres && break; sleep 1; done
tunr connect pg-role --config "$cfg" --database logo --tables fatura,stok --resource erp-db > /tmp/pg-role.sql
PGPASSWORD="$CUSTOMER_PG_OWNER_PASSWORD" psql -q -v ON_ERROR_STOP=1 -h customer-pg -U postgres -f /tmp/pg-role.sql
rm -f /tmp/pg-role.sql

exec tunr connect --config "$cfg"
