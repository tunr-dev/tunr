#!/usr/bin/env bash
# For recording the demo: start Acme's fake ERP database locally and open a
# shell on the "office server" (tunr CLI + psql, on the office network plus
# outbound internet). Inside it, paste the commands the dashboard shows, with
# --target acme-db:5432. The DBA password is in $ACME_DB_OWNER_PASSWORD.
#
#   ./scripts/demo/office-shell.sh         # start + shell
#   ./scripts/demo/office-shell.sh down    # remove containers and data
set -euo pipefail
cd "$(dirname "$0")"
export ACME_DB_OWNER_PASSWORD="${ACME_DB_OWNER_PASSWORD:-acme-dba-demo}"
# The always-on service needs these; the interactive shell does not use them.
export SITE_TOKEN="${SITE_TOKEN:-unused}" RELAY_KEY="${RELAY_KEY:-unused}"
P=(docker compose -p tunr-demo-local -f docker-compose.yml)

if [ "${1:-}" = down ]; then "${P[@]}" down -v; exit; fi
"${P[@]}" up -d --build acme-db
"${P[@]}" build acme-connector >/dev/null
exec "${P[@]}" run --rm --name acme-office-server -e PS1='acme-office-server:~# ' \
  acme-connector bash --norc -i
