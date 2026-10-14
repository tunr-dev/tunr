#!/bin/bash
# Connector container entrypoint: first-time setup from env, resources, run.
set -euo pipefail
cfg=/etc/tunr/connector.json
echo "$SITE_TOKEN" | tunr connect --config "$cfg" --relay "$RELAY_URL" --site "$SITE_NAME" \
  --token-stdin --relay-key "$RELAY_KEY" --secret-store file --setup-only
tunr connect resource add nas    --config "$cfg" --kind tcp --target customer-echo:7777
tunr connect resource add pg-raw --config "$cfg" --kind tcp --target customer-pg:5432
exec tunr connect --config "$cfg"
