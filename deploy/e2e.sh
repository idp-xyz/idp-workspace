#!/usr/bin/env bash
# End-to-end on the compose topology: the workspace image against PostgreSQL, driven over HTTP.
# From nothing each run: build image -> postgres -> migrations, service login, tenant -> workspace
# container -> a workspace, an app, an installation, an inbox item -> tear down (KEEP=1 leaves it up).
set -euo pipefail
cd "$(dirname "$0")/.."
ENVF=.local/compose.env
C=(docker compose -f deploy/compose.yaml --env-file "$ENVF" --profile tools)
WS=http://127.0.0.1:8560
mkdir -p .local
umask 077
if [[ ! -f "$ENVF" ]]; then
  { echo "IDP_PG_OWNER_PASSWORD=$(openssl rand -hex 16)"
    echo "IDP_APP_DB_PASSWORD=$(openssl rand -hex 16)"
    echo "IDP_DEV_SECRET=$(openssl rand -hex 32)"; } > "$ENVF"
fi
set -a; source "$ENVF"; set +a
bash deploy/build.sh idp-workspace:dev
"${C[@]}" down -v --remove-orphans >/dev/null 2>&1 || true
cleanup() { [[ "${KEEP:-0}" == 1 ]] || "${C[@]}" down -v --remove-orphans >/dev/null 2>&1; }
trap cleanup EXIT

"${C[@]}" up -d --wait postgres
"${C[@]}" exec -T postgres psql -U postgres -q -c 'CREATE DATABASE idp_workspace' >/dev/null
"${C[@]}" run --rm -T workspace-admin migrate
"${C[@]}" run --rm -T workspace-admin ensure-login idp_ws_svc
TENANT="$("${C[@]}" run --rm -T workspace-admin seed-tenant acme | sed -n 's/^TENANT_ID=//p')"
[[ -n "$TENANT" ]] || { echo "BLOCKED: no tenant" >&2; exit 2; }
tok() { "${C[@]}" run --rm -T workspace-admin dev-token "$TENANT" "$@"; }
ADMIN="$(tok alice ws-admin)"; MEMBER="$(tok bob ws-member)"; PRODUCER="$(tok svc-eer ws-producer)"
"${C[@]}" up -d workspace
for _ in $(seq 1 60); do curl -sf --noproxy '*' -o /dev/null "$WS/healthz" && break; sleep 0.5; done

call() { # call EXPECTED METHOD PATH TOKEN [BODY]
  local want="$1" method="$2" path="$3" token="$4" body="${5:-}" out status
  out="$(mktemp)"
  status="$(curl -s --noproxy '*' -o "$out" -w '%{http_code}' -X "$method" "$WS$path" -H "Authorization: Bearer $token" -H 'Content-Type: application/json' ${body:+--data "$body"})"
  if [[ "$status" != "$want" ]]; then echo "FAIL: $method $path -> $status (want $want): $(cat "$out")" >&2; exit 1; fi
  cat "$out"; rm -f "$out"
}
field() { python3 -c "import json,sys; print(json.load(sys.stdin)$1)"; }

call 201 POST /api/v1/workspaces "$ADMIN" '{"name":"us-ops","displayName":"US operations"}' >/dev/null
call 200 PUT /api/v1/workspaces/us-ops/members/bob "$ADMIN" '{"roles":["approver"]}' >/dev/null
call 201 POST /api/v1/apps "$ADMIN" '{"schemaVersion":"idp.workspace.app/v1","name":"notes","version":"1.0.0","title":"Notes","publisher":"IDP","entry":{"url":"https://notes.apps.example/"},"capabilities":["context.read"],"actions":[]}' >/dev/null
call 201 PUT /api/v1/workspaces/us-ops/installations/notes "$ADMIN" '{"version":"1.0.0"}' >/dev/null
[[ "$(call 200 GET /v1/workspaces/us-ops/apps "$MEMBER" | field "['apps'][0]['origin']")" == "https://notes.apps.example" ]] || { echo "FAIL: apps" >&2; exit 1; }
call 201 POST /v1/workspaces/us-ops/inbox/items "$PRODUCER" '{"externalRef":"execution:1/approval","kind":"approval","title":"Approve the refund","ref":{"system":"eer","kind":"execution","id":"execution:1"},"assignees":{"roles":["approver"]}}' >/dev/null
[[ "$(call 200 GET /v1/workspaces/us-ops/inbox "$MEMBER" | field "['items'][0]['title']")" == "Approve the refund" ]] || { echo "FAIL: inbox" >&2; exit 1; }
curl -sf --noproxy '*' http://127.0.0.1:9566/metrics | grep -q 'idp_workspace' || { echo "FAIL: metrics" >&2; exit 1; }
echo "PASS: workspace image e2e (tenant $TENANT)"
