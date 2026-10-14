#!/usr/bin/env bash
# Agent Bridge end-to-end test (roadmap §12, P3 scope).
#
#   ./scripts/e2e/bridge/run.sh          # build, run, assert, tear down
#   KEEP=1 ./scripts/e2e/bridge/run.sh   # leave the stack running afterwards
#
# Needs docker (compose v2), curl, jq, python3 (stdlib only, to mint test JWTs).
set -euo pipefail
cd "$(dirname "$0")"

DC="docker compose"
API=http://127.0.0.1:18090/api/v1/bridge
SECRET=e2e-only-secret-not-for-production-0123456789
pass() { printf '  \033[32m✓\033[0m %s\n' "$*"; }
fail() { printf '  \033[31m✗ %s\033[0m\n' "$*"; $DC logs --tail 40 relay connector >&2 || true; exit 1; }
step() { printf '\n\033[1m%s\033[0m\n' "$*"; }

cleanup() { [[ "${KEEP:-}" == 1 ]] || $DC --profile connector down -v --remove-orphans >/dev/null 2>&1 || true; }
trap cleanup EXIT

sql() { $DC exec -T relay-db psql -U tunr -d tunr -v ON_ERROR_STOP=1 -qtAc "$1"; }
jwt() { # human HS256 token for a user id + email
  python3 - "$1" "$2" "$SECRET" <<'PY'
import sys, hmac, hashlib, base64, json, time
b = lambda d: base64.urlsafe_b64encode(d).rstrip(b"=").decode()
uid, email, secret = sys.argv[1:4]
n = int(time.time())
h = b(json.dumps({"alg": "HS256", "typ": "JWT"}).encode())
p = b(json.dumps({"uid": uid, "email": email, "plan": "free", "iss": "tunr.sh", "sub": uid,
                  "iat": n, "nbf": n, "exp": n + 3600}).encode())
print(h + "." + p + "." + b(hmac.new(secret.encode(), (h + "." + p).encode(), hashlib.sha256).digest()))
PY
}
api() { # api TOKEN METHOD PATH [JSON] → body; fails on non-2xx
  local out code
  out=$(curl -sS -w '\n%{http_code}' -X "$2" -H "Authorization: Bearer $1" "$API$3" ${4:+-d "$4"})
  code=${out##*$'\n'}; out=${out%$'\n'*}
  [[ $code == 2* ]] || fail "$2 $3 → $code $out"
  echo "$out"
}
agent() { $DC exec -T -e TUNR_AGENT_TOKEN="$AGENT_TOKEN" agent bash -c "$1"; }

step "1. Build and start relay, relay-db, customer network"
$DC --profile connector down -v --remove-orphans >/dev/null 2>&1 || true
$DC build relay >/dev/null
$DC up -d relay-db relay customer-pg customer-echo agent >/dev/null
for _ in $(seq 60); do curl -fs "$API/health" >/dev/null 2>&1 && break; sleep 1; done
curl -fs "$API/health" >/dev/null || fail "relay never became healthy"
pass "relay healthy with TUNR_BRIDGE_ENABLED=1"

step "2. Orgs, link, site, agent (management API)"
CUID=$(sql "INSERT INTO users(email) VALUES ('it@acme.test') RETURNING id")
VUID=$(sql "INSERT INTO users(email) VALUES ('dev@vendor.test') RETURNING id")
CJ=$(jwt "$CUID" it@acme.test); VJ=$(jwt "$VUID" dev@vendor.test)
api "$CJ" POST /orgs '{"name":"Acme","slug":"acme","kind":"customer"}' >/dev/null
api "$VJ" POST /orgs '{"name":"Vendor","slug":"vendor-co","kind":"vendor"}' >/dev/null
api "$CJ" POST /orgs/acme/links '{"vendor_slug":"vendor-co"}' >/dev/null
SITE=$(api "$CJ" POST /orgs/acme/sites '{"name":"istanbul-ofis"}')
export SITE_TOKEN=$(jq -r .token <<<"$SITE") RELAY_KEY=$(jq -r .relay_key <<<"$SITE")
AGENT_ID=$(api "$VJ" POST /orgs/vendor-co/agents '{"name":"invoice-reconciler"}' | jq -r .id)
AGENT_TOKEN=$(api "$VJ" POST "/orgs/vendor-co/agents/$AGENT_ID/tokens" '{"ttl":"1h"}' | jq -r .token)
pass "customer acme linked vendor-co; site + agent created"

step "3. Connector comes online (outbound only)"
$DC --profile connector up -d connector >/dev/null
for _ in $(seq 30); do
  [[ $(api "$CJ" GET /orgs/acme/sites | jq -r '.sites[0].status') == online ]] && break; sleep 1
done
[[ $(api "$CJ" GET /orgs/acme/sites | jq -r '.sites[0].status') == online ]] || fail "site never came online"
RES=$(api "$CJ" GET /orgs/acme/resources | jq -r '[.resources[].name] | sort | join(",")')
[[ $RES == nas,pg-raw ]] || fail "resources = $RES"
pass "site online; resources announced: $RES"
DUMP=$($DC exec -T relay-db pg_dump -U tunr tunr)
grep -qE 'customer-echo|customer-pg|7777' <<<"$DUMP" && fail "a target address reached the relay database"
pass "no target address anywhere in the relay database"

step "4. No grant → refused before any stream"
OUT=$(agent 'tunr access acme/nas --port 14455 2>&1' || true)
grep -q no_grant <<<"$OUT" || fail "expected no_grant, got: $OUT"
pass "tunr access refused: no_grant"

grant() { # mode → approves a grant directly (approval API arrives in P4)
  sql "UPDATE grants SET status='revoked' WHERE agent_id='$AGENT_ID' AND status='approved';
       INSERT INTO grants (id, agent_id, resource_id, vendor_org_id, customer_org_id, mode, status,
                           requested_ttl_secs, approved_at, expires_at)
       SELECT 'grt_e2e_' || substr(md5(random()::text), 1, 10), a.id, r.id, a.org_id, r.org_id, '$1',
              'approved', 3600, now(), now() + interval '1 hour'
       FROM agents a, resources r WHERE a.id='$AGENT_ID' AND r.name IN ('nas','pg-raw')" >/dev/null
}

step "5. ro grant on a raw TCP resource → connector refuses (TCP can't be read-only)"
grant ro
OUT=$(agent 'tunr access acme/nas --port 14455 >/tmp/a.log 2>&1 & sleep 1;
             printf hello | nc -w 3 127.0.0.1 14455; echo "[rc=$?]"; sleep 0.5; cat /tmp/a.log; kill %1' 2>&1 || true)
grep -q mode_not_enforceable <<<"$OUT" || fail "expected mode_not_enforceable: $OUT"
grep -q HELLO <<<"$OUT" && fail "data flowed on an ro tcp grant"
pass "stream refused by the connector: mode_not_enforceable"

step "6. rw grant → bytes and a real Postgres query through the bridge"
grant rw
OUT=$(agent 'tunr access acme/nas --port 14455 >/dev/null 2>&1 & sleep 1;
             printf "select 1" | nc -w 2 127.0.0.1 14455; kill %1' || true)
[[ $OUT == "SELECT 1"* ]] || fail "echo through bridge = '$OUT'"
pass "tcp echo round trip: '$OUT'"
N=$(agent 'tunr access acme/pg-raw --port 15432 >/dev/null 2>&1 & sleep 1;
           PGPASSWORD=owner-password psql -h 127.0.0.1 -p 15432 -U postgres -d logo -tAc "select count(*) from fatura";
           kill %1' || true)
[[ $N == 42 ]] || fail "select count(*) from fatura through bridge = '$N'"
pass "psql through the bridge: 42 invoices (raw tcp; read-only postgres arrives in P5)"

step "7. Network isolation"
agent 'nc -z -w 2 customer-pg 5432' >/dev/null 2>&1 && fail "agent reached customer-pg directly"
$DC exec -T relay sh -c 'nc -z -w 2 customer-pg 5432' >/dev/null 2>&1 && fail "relay reached customer-pg directly"
pass "agent and relay cannot reach the customer network directly"

step "8. Audit trail"
EV=$(sql "SELECT count(*) FROM audit_log a JOIN orgs o ON o.id = a.org_id
          WHERE o.slug='acme' AND a.action IN ('bridge.session_opened','bridge.session_closed','bridge.site_connected')")
(( EV >= 5 )) || fail "customer audit events = $EV"
SESS=$(sql "SELECT count(*) FROM access_sessions WHERE closed_at IS NOT NULL AND bytes_down > 0")
(( SESS >= 2 )) || fail "closed sessions with traffic = $SESS"
pass "$EV customer audit events; $SESS sessions with byte counts"

printf '\n\033[32mbridge e2e: all checks passed\033[0m\n'
