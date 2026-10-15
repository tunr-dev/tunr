#!/usr/bin/env bash
# Agent Bridge end-to-end test (roadmap §12, P3 scope).
#
#   ./scripts/e2e/bridge/run.sh          # build, run, assert, tear down
#   KEEP=1 ./scripts/e2e/bridge/run.sh   # leave the stack running afterwards
#
# Needs docker (compose v2), curl, jq, python3 (stdlib only, to mint test JWTs), awk.
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
agentd() { $DC exec -d -T -e TUNR_AGENT_TOKEN="$AGENT_TOKEN" agent bash -c "$1"; } # detached: outlives this exec
human() { $DC exec -T -e TUNR_TOKEN="$1" agent bash -c "$2"; } # customer CLI (runs in any box with the binary)

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
C2UID=$(sql "INSERT INTO users(email) VALUES ('cfo@acme.test') RETURNING id"); C2J=$(jwt "$C2UID" cfo@acme.test)
api "$CJ" POST /orgs/acme/members '{"email":"cfo@acme.test","role":"approver"}' >/dev/null
pass "customer acme linked vendor-co; site + agent created; second approver added"

step "3. Connector comes online (outbound only)"
$DC --profile connector up -d connector >/dev/null
for _ in $(seq 30); do
  [[ $(api "$CJ" GET /orgs/acme/sites | jq -r '.sites[0].status') == online ]] && break; sleep 1
done
[[ $(api "$CJ" GET /orgs/acme/sites | jq -r '.sites[0].status') == online ]] || fail "site never came online"
RES=$(api "$CJ" GET /orgs/acme/resources | jq -r '[.resources[].name] | sort | join(",")')
[[ $RES == erp-db,nas ]] || fail "resources = $RES"
pass "site online; resources announced: $RES"
H=$($DC exec -T connector curl -fsS 127.0.0.1:9095/healthz) || fail "connector /healthz"
[[ $(jq -r .connected <<<"$H") == true ]] || fail "healthz: $H"
pass "connector /healthz reports connected"
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
       FROM agents a, resources r WHERE a.id='$AGENT_ID' AND r.name = 'nas'" >/dev/null
}

step "5. ro grant on a raw TCP resource → connector refuses (TCP can't be read-only)"
grant ro
OUT=$(agent 'tunr access acme/nas --port 14455 >/tmp/a.log 2>&1 & sleep 1;
             printf hello | nc -w 3 127.0.0.1 14455; echo "[rc=$?]"; sleep 0.5; cat /tmp/a.log; kill %1' 2>&1 || true)
grep -q mode_not_enforceable <<<"$OUT" || fail "expected mode_not_enforceable: $OUT"
grep -q HELLO <<<"$OUT" && fail "data flowed on an ro tcp grant"
pass "stream refused by the connector: mode_not_enforceable"

step "6. Grant flow: request → two approvals → traffic"
OUT=$(agent 'tunr catalog' 2>&1)
grep -q "acme/nas" <<<"$OUT" || fail "catalog: $OUT"
OUT=$(agent 'tunr access request acme/nas --mode ro --ttl 1h --reason "x" 2>&1' || true)
grep -q mode_not_enforceable <<<"$OUT" || fail "relay accepted ro on tcp: $OUT"
pass "relay policy refuses ro on raw tcp before anyone is asked"
sql "UPDATE grants SET status='revoked' WHERE status='approved'" >/dev/null
GID=$(agent 'tunr access request acme/nas --ttl 1h --reason "Nightly export of shared files"' | grep -o 'grt_[0-9a-f]*' | head -1)
[[ -n $GID ]] || fail "no grant id from tunr access request"
human "$CJ" "tunr grants approve $GID --org acme" | grep -q "pending" || fail "first rw approval should leave it pending"
OUT=$(human "$CJ" "tunr grants approve $GID --org acme 2>&1" || true)
grep -q same_approver <<<"$OUT" || fail "same approver twice was accepted: $OUT"
human "$C2J" "tunr grants approve $GID --org acme" | grep -q approved || fail "second approval did not approve"
pass "rw grant $GID approved by two different people (same person twice refused)"
OUT=$(agent 'tunr access acme/nas --port 14455 >/dev/null 2>&1 & sleep 1;
             printf "select 1" | nc -w 2 127.0.0.1 14455; kill %1' || true)
[[ $OUT == "SELECT 1"* ]] || fail "echo through bridge = '$OUT'"
pass "tcp echo round trip: '$OUT'"

step "6b. Revoke cuts an open connection (≤ 2 s)"
agentd 'tunr access acme/nas --port 14456 >/tmp/rv.log 2>&1'
sleep 1
# cat exits when the socket reaches EOF (nc would wait for its stdin).
agentd 'exec 3<>/dev/tcp/127.0.0.1/14456; cat <&3 >/dev/null; echo $EPOCHREALTIME > /tmp/closed_at'
sleep 1.5
agent 'test ! -f /tmp/closed_at' || fail "connection closed before the revoke"
human "$CJ" "echo \$EPOCHREALTIME > /tmp/t0; tunr grants revoke $GID --org acme" | grep -q revoked || fail "revoke failed"
for _ in $(seq 40); do agent 'test -f /tmp/closed_at' && break; sleep 0.1; done
agent 'test -f /tmp/closed_at' || fail "open connection survived revocation"
DT=$(agent 'awk -v a="$(cat /tmp/t0)" -v b="$(cat /tmp/closed_at)" "BEGIN{printf \"%.3f\", b-a}"')
awk -v d="$DT" 'BEGIN{exit !(d <= 2.0)}' || fail "revocation took ${DT}s (> 2s)"
pass "connection closed ${DT}s after revoke"
OUT=$(agent 'tunr access acme/nas --port 14457 2>&1' || true)
grep -q no_grant <<<"$OUT" || fail "revoked grant still usable: $OUT"
pass "revoked grant can't be used again"

step "6c. Customer kill switch on the site"
api "$CJ" POST /orgs/acme/sites/$(api "$CJ" GET /orgs/acme/sites | jq -r '.sites[0].id')/kill >/dev/null
[[ $(api "$CJ" GET "/orgs/acme/grants?status=approved" | jq '.grants | length') == 0 ]] || fail "approved grants survived kill"
pass "kill revoked every approved grant on the site"

step "7. Read-only Postgres: the agent never sees the DB password"
api "$CJ" PATCH /orgs/acme/resources/$(api "$CJ" GET /orgs/acme/resources | jq -r '.resources[] | select(.name=="erp-db") | .id') \
  '{"description":"Logo ERP (read replica)","limits":{"max_rows":20}}' >/dev/null
PG=$(agent 'tunr access request acme/erp-db --ttl 30m --reason "Reconcile October invoices"' | grep -o 'grt_[0-9a-f]*' | head -1)
human "$CJ" "tunr grants approve $PG --org acme" | grep -q approved || fail "ro grant needs one approver"
agentd 'tunr access acme/erp-db --port 15432 > /tmp/dsn.txt 2>/tmp/pg.log'
for _ in $(seq 30); do agent 'grep -q postgresql:// /tmp/dsn.txt' && break; sleep 0.3; done
DSN=$(agent 'grep postgresql:// /tmp/dsn.txt')
[[ $DSN == postgresql://agent:*@127.0.0.1:15432/* ]] || fail "dsn = '$DSN'"
grep -q owner-password <<<"$DSN" && fail "DSN contains the database password"
pass "tunr access printed a local DSN; no database credential in it"
agent "psql '$DSN' -tAc 'select 1' >/dev/null"
agent 'grep -q "end-to-end encrypted" /tmp/dsn.txt' || fail "stream was not end-to-end encrypted: $(agent 'cat /tmp/dsn.txt /tmp/pg.log')"
FP=$(api "$CJ" GET /orgs/acme/sites | jq -r '.sites[0].tls_fingerprint')
[[ ${#FP} == 64 ]] || fail "site TLS fingerprint not published: $FP"
pass "stream is end-to-end TLS to the connector (relay carries ciphertext); site fingerprint ${FP:0:16}…"
N=$(agent "psql '$DSN' -tAc 'select count(*) from fatura where tutar > 0'")
[[ $N == 42 ]] || fail "select through pgproxy = '$N'"
pass "SELECT through the read-only proxy: $N invoices"
OUT=$(agent "psql '$DSN' -tAc \"update fatura set tutar = 0\" 2>&1" || true)
grep -q "read-only" <<<"$OUT" || fail "update not refused: $OUT"
OUT=$(agent "psql '$DSN' -tAc \"select 1; delete from fatura\" 2>&1" || true)
grep -q "read-only" <<<"$OUT" || fail "stacked delete not refused: $OUT"
N=$($DC exec -T -e PGPASSWORD=owner-password customer-pg psql -h 127.0.0.1 -U postgres -d logo -tAc "select count(*) from fatura where tutar > 0")
[[ $N == 42 ]] || fail "customer data changed: $N"
pass "UPDATE and stacked DELETE refused (42501); customer data untouched"
OUT=$(agent "psql '$DSN' -tAc 'select * from fatura' 2>&1" || true)
grep -q "row limit (20)" <<<"$OUT" || fail "max_rows not enforced: $OUT"
pass "row limit enforced (42 rows > max_rows 20)"
OUT=$(agent "psql 'postgresql://agent:wrong@127.0.0.1:15432/db?sslmode=disable' -tAc 'select 1' 2>&1" || true)
grep -q "password authentication failed" <<<"$OUT" || fail "wrong local password accepted: $OUT"
pass "a local client without the DSN secret is refused"
summaries() {
  local s
  for s in $(api "$CJ" GET "/orgs/acme/grants/$PG/sessions" | jq -r '.sessions[].id'); do
    api "$CJ" GET "/orgs/acme/sessions/$s/queries" | jq -r '.queries[] | "\(.command) \(.tables|join(",")) \(.row_count)"'
  done
}
for _ in $(seq 30); do Q=$(summaries); (( $(grep -c . <<<"$Q") >= 4 )) && break; sleep 0.2; done # stored asynchronously
grep -q "^SELECT fatura 1" <<<"$Q" || fail "no SELECT summary: $Q"
grep -q "^blocked:UPDATE fatura" <<<"$Q" || fail "no blocked UPDATE summary: $Q"
pass "customer sees per-query summaries: $(grep -c . <<<"$Q") queries, $(grep -c '^blocked' <<<"$Q") blocked"

step "8. Network isolation"
agent 'nc -z -w 2 customer-pg 5432' >/dev/null 2>&1 && fail "agent reached customer-pg directly"
$DC exec -T relay sh -c 'nc -z -w 2 customer-pg 5432' >/dev/null 2>&1 && fail "relay reached customer-pg directly"
pass "agent and relay cannot reach the customer network directly"

step "9. Audit trail"
EV=$(sql "SELECT count(*) FROM audit_log a JOIN orgs o ON o.id = a.org_id
          WHERE o.slug='acme' AND a.action IN ('bridge.session_opened','bridge.session_closed','bridge.site_connected',
          'bridge.grant_requested','bridge.grant_first_approval','bridge.grant_approved','bridge.grant_revoked','bridge.kill',
          'bridge.query_blocked')")
(( EV >= 12 )) || fail "customer audit events = $EV"
api "$CJ" GET "/orgs/acme/audit/export?format=csv" >/dev/null
SESS=$(sql "SELECT count(*) FROM access_sessions WHERE closed_at IS NOT NULL AND bytes_down > 0")
(( SESS >= 2 )) || fail "closed sessions with traffic = $SESS"
pass "$EV customer audit events (exportable as CSV); $SESS sessions with byte counts"

printf '\n\033[32mbridge e2e: all checks passed\033[0m\n'
