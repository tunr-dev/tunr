#!/usr/bin/env bash
# Hosted-agent end-to-end test (roadmap P7): a real runner builds and runs an
# agent; a signed webhook and a cron trigger wake it; it reaches the customer's
# ERP read-only with its own identity and writes to its /data volume.
#
#   ./scripts/e2e/bridge/hosted.sh        KEEP=1 to leave it running
#
# Needs docker, curl, jq, python3, openssl, go. Uses the host Docker daemon
# with runc (gVisor is the production runtime). Creates the docker network
# tunr-apps if missing.
set -euo pipefail
cd "$(dirname "$0")"
ROOT=$(cd ../../.. && pwd)

DC="docker compose -f docker-compose.yml -f docker-compose.hosted.yml"
RELAY=http://127.0.0.1:18090
API=$RELAY/api/v1/bridge
SECRET=e2e-only-secret-not-for-production-0123456789
pass() { printf '  \033[32m✓\033[0m %s\n' "$*"; }
fail() { printf '  \033[31m✗ %s\033[0m\n' "$*"; $DC logs --tail 30 relay runner >&2 || true; exit 1; }
step() { printf '\n\033[1m%s\033[0m\n' "$*"; }

cleanup() {
  [[ "${KEEP:-}" == 1 ]] && return
  docker ps -aq --filter label=tunr.app | xargs -r docker rm -f >/dev/null 2>&1 || true
  docker volume ls -q --filter name=tunr-vol- | xargs -r docker volume rm -f >/dev/null 2>&1 || true
  $DC --profile connector down -v --remove-orphans >/dev/null 2>&1 || true
  docker network rm tunr-apps >/dev/null 2>&1 || true
}
trap cleanup EXIT

sql() { $DC exec -T relay-db psql -U tunr -d tunr -v ON_ERROR_STOP=1 -qtAc "$1"; }
jwt() {
  python3 - "$1" "$2" "$SECRET" <<'PY'
import sys, hmac, hashlib, base64, json, time
b = lambda d: base64.urlsafe_b64encode(d).rstrip(b"=").decode()
uid, email, secret = sys.argv[1:4]; n = int(time.time())
h = b(json.dumps({"alg": "HS256", "typ": "JWT"}).encode())
p = b(json.dumps({"uid": uid, "email": email, "plan": "free", "iss": "tunr.sh", "sub": uid, "iat": n, "nbf": n, "exp": n + 3600}).encode())
print(h + "." + p + "." + b(hmac.new(secret.encode(), (h + "." + p).encode(), hashlib.sha256).digest()))
PY
}
api() {
  local out code
  out=$(curl -sS -w '\n%{http_code}' -X "$2" -H "Authorization: Bearer $1" "$API$3" ${4:+-d "$4"})
  code=${out##*$'\n'}; out=${out%$'\n'*}
  [[ $code == 2* ]] || fail "$2 $3 → $code $out"
  echo "$out"
}
hook() { # url-host secret → JSON response
  local body='{"source":"e2e"}' ts sig
  ts=$(date +%s)
  sig=$(printf '%s.%s' "$ts" "$body" | openssl dgst -sha256 -hmac "$2" -hex | awk '{print $NF}')
  curl -sS -X POST "$RELAY/__tunr/hooks/run-now" -H "Host: $1" -H "X-Tunr-Signature: t=$ts,v1=$sig" -d "$body"
}

step "1. Build the agent-side tunr (linux) and the stack"
case "$(docker info --format '{{.Architecture}}')" in aarch64|arm64) ARCH=arm64 ;; *) ARCH=amd64 ;; esac
mkdir -p .build
(cd "$ROOT" && CGO_ENABLED=0 GOOS=linux GOARCH=$ARCH go build -trimpath -o scripts/e2e/bridge/.build/tunr ./cmd/tunr)
(cd "$ROOT" && CGO_ENABLED=0 go build -trimpath -o scripts/e2e/bridge/.build/tunr-host ./cmd/tunr)
export TUNR_AGENT_BINARY=$PWD/.build/tunr
docker network create tunr-apps >/dev/null 2>&1 || true
$DC --profile connector down -v --remove-orphans >/dev/null 2>&1 || true
$DC build relay runner >/dev/null
$DC up -d relay-db relay customer-pg customer-echo runner >/dev/null
for _ in $(seq 60); do curl -fs "$API/health" >/dev/null 2>&1 && break; sleep 1; done
curl -fs "$API/health" >/dev/null || fail "relay not healthy"
pass "relay + runner (runc) up"

step "2. Customer, vendor, site, connector, agent"
CJ=$(jwt "$(sql "INSERT INTO users(email) VALUES ('it@acme.test') RETURNING id")" it@acme.test)
VJ=$(jwt "$(sql "INSERT INTO users(email) VALUES ('dev@vendor.test') RETURNING id")" dev@vendor.test)
api "$CJ" POST /orgs '{"name":"Acme","slug":"acme","kind":"customer"}' >/dev/null
api "$VJ" POST /orgs '{"name":"Vendor","slug":"vendor-co","kind":"vendor"}' >/dev/null
api "$CJ" POST /orgs/acme/links '{"vendor_slug":"vendor-co"}' >/dev/null
SITE=$(api "$CJ" POST /orgs/acme/sites '{"name":"istanbul-ofis"}')
export SITE_TOKEN=$(jq -r .token <<<"$SITE") RELAY_KEY=$(jq -r .relay_key <<<"$SITE")
$DC --profile connector up -d connector >/dev/null
for _ in $(seq 40); do [[ $(api "$CJ" GET /orgs/acme/sites | jq -r '.sites[0].status') == online ]] && break; sleep 1; done
AGENT_ID=$(api "$VJ" POST /orgs/vendor-co/agents '{"name":"invoice-reconciler"}' | jq -r .id)
G=$(api "$VJ" POST /orgs/vendor-co/grants "{\"agent_id\":\"$AGENT_ID\",\"resource\":\"acme/erp-db\",\"ttl\":\"2h\",\"reason\":\"Nightly invoice reconciliation\"}" | jq -r .id)
api "$CJ" POST "/orgs/acme/grants/$G/approve" >/dev/null
pass "site online; read-only grant on acme/erp-db approved for the agent"

step "3. Deploy the agent on tunr"
OUT=$(TUNR_TOKEN=$VJ TUNR_RELAY_URL=$RELAY .build/tunr-host agent deploy invoice-reconciler --org vendor-co --volume 64 agent 2>&1) \
  || fail "deploy: $OUT"
grep -q "is running on tunr" <<<"$OUT" || fail "deploy: $OUT"
APP_ID=$(sql "SELECT id FROM apps WHERE name='invoice-reconciler'")
docker exec "tunr-app-$APP_ID" sh -c 'test -x /opt/tunr/bin/tunr && test -d /data && ! touch /etc/x 2>/dev/null' \
  || fail "agent container: tunr binary, /data or read-only root missing"
pass "agent running ($APP_ID): tunr mounted read-only, /data volume, read-only root"
HOST=invoice-reconciler.bridge.test
[[ $(curl -s -o /dev/null -w '%{http_code}' -H "Host: $HOST" "$RELAY/") == 404 ]] || fail "agent is publicly reachable"
pass "agent is not public"

step "4. Signed webhook → agent reads the ERP with its own identity"
TR=$(TUNR_TOKEN=$VJ TUNR_RELAY_URL=$RELAY .build/tunr-host agent trigger add invoice-reconciler --org vendor-co --webhook run-now 2>&1)
HSECRET=$(grep -o 'whsec_[A-Za-z0-9_-]*' <<<"$TR" | head -1)
[[ -n $HSECRET ]] || fail "trigger add: $TR"
[[ $(hook "$HOST" whsec_wrong | tr -d '\n') == *"bad or missing"* ]] || fail "wrongly signed hook accepted"
R=$(hook "$HOST" "$HSECRET")
[[ $(jq -r .agent_status <<<"$R") == 200 ]] || fail "hook: $R"
LOG=$(docker exec "tunr-app-$APP_ID" cat /data/runs.log)
grep -q "webhook:run-now 42" <<<"$LOG" || fail "agent result: $LOG"
pass "webhook delivered; agent counted 42 invoices through the bridge (no DB password, no token in env)"

step "5. Sleep, then wake on the next trigger"
for _ in $(seq 60); do [[ $(docker inspect -f '{{.State.Paused}}' "tunr-app-$APP_ID") == true ]] && break; sleep 1; done
[[ $(docker inspect -f '{{.State.Paused}}' "tunr-app-$APP_ID") == true ]] || fail "agent never went to sleep"
pass "agent paused after idling"
R=$(hook "$HOST" "$HSECRET")
[[ $(jq -r .agent_status <<<"$R") == 200 ]] || fail "hook after sleep: $R"
[[ $(docker exec "tunr-app-$APP_ID" grep -c "webhook:run-now 42" /data/runs.log) == 2 ]] || fail "second run missing"
pass "woke on the trigger, ran again; /data kept the first run"

step "6. Cron trigger"
TUNR_TOKEN=$VJ TUNR_RELAY_URL=$RELAY .build/tunr-host agent trigger add invoice-reconciler --org vendor-co --cron "* * * * *" >/dev/null
for _ in $(seq 100); do docker exec "tunr-app-$APP_ID" grep -q "cron:schedule 42" /data/runs.log 2>/dev/null && break; sleep 1; done
docker exec "tunr-app-$APP_ID" grep -q "cron:schedule 42" /data/runs.log || fail "cron never fired"
pass "cron fired and the agent did its job"

step "7. Audit"
N=$(sql "SELECT count(*) FROM audit_log WHERE action = 'bridge.trigger_fired'")
S=$(sql "SELECT count(*) FROM access_sessions s JOIN grants g ON g.id = s.grant_id WHERE g.agent_id = '$AGENT_ID'")
(( N >= 3 && S >= 3 )) || fail "trigger_fired=$N agent sessions=$S"
pass "$N triggers and $S agent sessions on record"

printf '\n\033[32mhosted agent e2e: all checks passed\033[0m\n'
