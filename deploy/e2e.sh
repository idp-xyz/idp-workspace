#!/usr/bin/env bash
# End-to-end on the compose topology: the workspace image against PostgreSQL, driven over HTTP.
# From nothing each run: build image -> postgres -> migrations, service login, tenant -> workspace
# container -> a workspace, an app, an installation, an inbox item, the web shell -> tear down
# (KEEP=1 leaves it up).
#   HOST=1   also start the idp-host image and its model double (built in the idp-host repository:
#            deploy/build.sh idp-host:dev and deploy/fixtures/build-fakemodel.sh); the workspace calls
#            it with a static HOST token, and the test proves ADR-0001 D4 / D11 / D12 between real
#            processes: a member's host.run action starts a run whose result comes back into the inbox.
#   OIDC=1   (implies HOST=1) Keycloak instead of development tokens (deploy/keycloak/realm.py): the
#            workspace and the HOST verify Keycloak's tokens, the workspace exchanges a member's token
#            for the HOST's audience (ADR-0001 D14, D16), and with BROWSER=1 the shell signs in through
#            Keycloak's login page (tests/browser/oidc.smoke.mjs).
#   BROWSER=1  also drive the web shell in a real Chromium (tests/browser/shell.smoke.mjs) with the
#            repository's playwright-core and its Chromium (npx playwright-core install chromium), or
#            PLAYWRIGHT_CORE / CHROMIUM when given.
set -euo pipefail
cd "$(dirname "$0")/.."
ENVF=.local/compose.env
OIDC="${OIDC:-0}"
HOST="${HOST:-0}"
[[ "$OIDC" == 1 ]] && HOST=1
C=(docker compose -f deploy/compose.yaml --env-file "$ENVF" --profile tools --profile host-tools)
[[ "$HOST" == 1 ]] && C+=(--profile host)
[[ "$OIDC" == 1 ]] && C+=(--profile oidc)
ISSUER=http://127.0.0.1:7080/realms/idp
KC_TOKEN="$ISSUER/protocol/openid-connect/token"
WS=http://127.0.0.1:8560
HOSTURL=http://127.0.0.1:8450
mkdir -p .local
umask 077
if [[ ! -f "$ENVF" ]] || ! grep -q IDP_HOST_DEV_SECRET "$ENVF"; then
  { echo "IDP_PG_OWNER_PASSWORD=$(openssl rand -hex 16)"
    echo "IDP_APP_DB_PASSWORD=$(openssl rand -hex 16)"
    echo "IDP_HOST_DB_PASSWORD=$(openssl rand -hex 16)"
    echo "IDP_DEV_SECRET=$(openssl rand -hex 32)"
    echo "IDP_HOST_DEV_SECRET=$(openssl rand -hex 32)"
    echo "IDP_HOST_CREDENTIAL_KEY=$(openssl rand -hex 32)"
    echo "KC_BOOTSTRAP_ADMIN_PASSWORD=$(openssl rand -hex 16)"; } > "$ENVF"
fi
grep -q '^KC_BOOTSTRAP_ADMIN_PASSWORD=' "$ENVF" || echo "KC_BOOTSTRAP_ADMIN_PASSWORD=$(openssl rand -hex 16)" >> "$ENVF"
set -a; source "$ENVF"; set +a
if [[ "$HOST" == 1 ]]; then
  for img in "${IDP_HOST_IMAGE:-idp-host:dev}" "${IDP_FAKEMODEL_IMAGE:-idp-host-fakemodel:dev}"; do
    docker image inspect "$img" >/dev/null 2>&1 || { echo "BLOCKED: image $img missing (build it in the idp-host repository)" >&2; exit 2; }
  done
fi
bash deploy/build.sh idp-workspace:dev
"${C[@]}" down -v --remove-orphans >/dev/null 2>&1 || true
cleanup() { [[ "${KEEP:-0}" == 1 ]] || "${C[@]}" down -v --remove-orphans >/dev/null 2>&1; }
trap cleanup EXIT

# the workspace container mounts these; without HOST=1 the config is empty (no targets)
umask 022
: > .local/workspace.yaml; printf 'none\n' > .local/host.key; printf 'none\n' > .local/exchange.secret; chmod 644 .local/workspace.yaml .local/host.key .local/exchange.secret
mkdir -p .local/keycloak; chmod 755 .local/keycloak
if [[ "$OIDC" == 1 ]]; then
  TENANT_FIXED="$(python3 -c 'import uuid; print(uuid.uuid4())')"
  rm -rf .local/keycloak/*
  python3 deploy/keycloak/realm.py .local/keycloak "$TENANT_FIXED" .local/keycloak-clients.json >/dev/null
  python3 -c 'import json; print(json.load(open(".local/keycloak-clients.json"))["idp-workspace"])' > .local/exchange.secret
  export IDP_WORKSPACE_JWKS_URL=http://keycloak:7080/realms/idp/protocol/openid-connect/certs IDP_WORKSPACE_ISSUER="$ISSUER" \
    IDP_WORKSPACE_OIDC_CLIENT_ID=workspace-shell IDP_WORKSPACE_SUBJECT_CLAIM=preferred_username \
    IDP_HOST_JWKS_URL=http://keycloak:7080/realms/idp/protocol/openid-connect/certs IDP_HOST_ISSUER="$ISSUER"
fi
printf 'providers:\n  - name: local\n    kind: openai\n    baseUrl: http://fakemodel:18080/v1\n    insecure: true\n' > .local/host.yaml; chmod 644 .local/host.yaml
umask 077

"${C[@]}" up -d --wait postgres
if [[ "$OIDC" == 1 ]]; then
  "${C[@]}" up -d keycloak
  for _ in $(seq 1 120); do curl -sf --noproxy '*' -o /dev/null "$ISSUER/.well-known/openid-configuration" && break; sleep 1; done
  curl -sf --noproxy '*' -o /dev/null "$ISSUER/.well-known/openid-configuration" || { "${C[@]}" logs --tail 30 keycloak; echo "BLOCKED: Keycloak did not come up" >&2; exit 2; }
fi
secret_of() { python3 -c "import json,sys; d=json.load(open('.local/keycloak-clients.json')); print(d['users'][sys.argv[1]] if sys.argv[1] in d['users'] else d[sys.argv[1]])" "$1"; }
kc_user() { curl -sf --noproxy '*' "$KC_TOKEN" -d grant_type=password -d client_id=e2e-cli -d "client_secret=$(secret_of e2e-cli)" -d "username=$1" -d "password=$(secret_of "$1")" -d scope=openid | python3 -c 'import json,sys; print(json.load(sys.stdin)["access_token"])'; }
kc_service() { curl -sf --noproxy '*' "$KC_TOKEN" -d grant_type=client_credentials -d "client_id=$1" -d "client_secret=$(secret_of "$1")" | python3 -c 'import json,sys; print(json.load(sys.stdin)["access_token"])'; }
"${C[@]}" exec -T postgres psql -U postgres -q -c 'CREATE DATABASE idp_workspace' >/dev/null
"${C[@]}" run --rm -T workspace-admin migrate
"${C[@]}" run --rm -T workspace-admin ensure-login idp_ws_svc
TENANT="$("${C[@]}" run --rm -T workspace-admin seed-tenant acme ${TENANT_FIXED:-} | sed -n 's/^TENANT_ID=//p')"
[[ -n "$TENANT" ]] || { echo "BLOCKED: no tenant" >&2; exit 2; }
tok() { "${C[@]}" run --rm -T workspace-admin dev-token "$TENANT" "$@"; }
if [[ "$OIDC" == 1 ]]; then
  ADMIN="$(kc_user alice)"; MEMBER="$(kc_user bob)"; PRODUCER="$(kc_service svc-eer)"
else
  ADMIN="$(tok alice ws-admin)"; MEMBER="$(tok bob ws-member)"; PRODUCER="$(tok svc-eer ws-producer)"
fi

if [[ "$HOST" == 1 ]]; then
  "${C[@]}" exec -T postgres psql -U postgres -q -c 'CREATE DATABASE idp_host' >/dev/null
  "${C[@]}" run --rm -T host-admin migrate
  "${C[@]}" run --rm -T host-admin ensure-login idp_host_svc
  HTENANT="$("${C[@]}" run --rm -T host-admin seed-tenant acme "$TENANT" | sed -n 's/^TENANT_ID=//p')"
  [[ "$HTENANT" == "$TENANT" ]] || { echo "BLOCKED: HOST tenant $HTENANT != workspace tenant $TENANT" >&2; exit 2; }
  htok() { "${C[@]}" run --rm -T host-admin dev-token "$TENANT" "$@"; }
  umask 022
  if [[ "$OIDC" == 1 ]]; then
    H_ADMIN="$(kc_user hostadmin)"; H_APPROVER="$(kc_user hostapprover)"
    # D16: the member's own token, exchanged at Keycloak for the HOST's audience
    printf 'targets:\n  host:\n    endpoint: "http://host:8450"\n    auth: exchange\n    exchange: {tokenUrl: "http://keycloak:7080/realms/idp/protocol/openid-connect/token", clientId: idp-workspace, clientSecretFile: /etc/idp/exchange.secret, audience: idp-host}\n' > .local/workspace.yaml
  else
    H_ADMIN="$(htok alice host-admin)"; H_APPROVER="$(htok carol host-approver)"
    htok idp-workspace host-user > .local/host.key; chmod 644 .local/host.key
    printf 'targets:\n  host: {endpoint: "http://host:8450", auth: static, tokenFile: /etc/idp/host.key}\n' > .local/workspace.yaml
  fi
  umask 077
  "${C[@]}" up -d fakemodel host host-worker
  for _ in $(seq 1 60); do curl -sf --noproxy '*' -o /dev/null "$HOSTURL/healthz" && break; sleep 0.5; done
  curl -sf --noproxy '*' -o /dev/null "$HOSTURL/healthz" || { "${C[@]}" logs --tail 20 host host-worker; echo "BLOCKED: the HOST did not come up" >&2; exit 2; }
fi
"${C[@]}" up -d workspace
for _ in $(seq 1 60); do curl -sf --noproxy '*' -o /dev/null "$WS/healthz" && break; sleep 0.5; done

call() { # call EXPECTED METHOD URL TOKEN [BODY]
  local want="$1" method="$2" url="$3" token="$4" body="${5:-}" out status
  out="$(mktemp)"
  status="$(curl -s --noproxy '*' -o "$out" -w '%{http_code}' -X "$method" "$url" -H "Authorization: Bearer $token" -H 'Content-Type: application/json' ${body:+--data "$body"})"
  if [[ "$status" != "$want" ]]; then echo "FAIL: $method $url -> $status (want $want): $(cat "$out")" >&2; rm -f "$out"; exit 1; fi
  cat "$out"; rm -f "$out"
}
field() { python3 -c "import json,sys; print(json.load(sys.stdin)$1)"; }

call 201 POST "$WS/api/v1/workspaces" "$ADMIN" '{"name":"us-ops","displayName":"US operations"}' >/dev/null
call 200 PUT "$WS/api/v1/workspaces/us-ops/members/bob" "$ADMIN" '{"roles":["approver"]}' >/dev/null
call 201 POST "$WS/api/v1/apps" "$ADMIN" '{"schemaVersion":"idp.workspace.app/v1","name":"notes","version":"1.0.0","title":"Notes","publisher":"IDP","entry":{"url":"https://notes.apps.example/"},"capabilities":["context.read"],"actions":[]}' >/dev/null
call 201 PUT "$WS/api/v1/workspaces/us-ops/installations/notes" "$ADMIN" '{"version":"1.0.0"}' >/dev/null
[[ "$(call 200 GET "$WS/v1/workspaces/us-ops/apps" "$MEMBER" | field "['apps'][0]['origin']")" == "https://notes.apps.example" ]] || { echo "FAIL: apps" >&2; exit 1; }
call 201 POST "$WS/v1/workspaces/us-ops/inbox/items" "$PRODUCER" '{"externalRef":"execution:1/approval","kind":"approval","title":"Approve the refund","ref":{"system":"eer","kind":"execution","id":"execution:1"},"assignees":{"roles":["approver"]}}' >/dev/null
[[ "$(call 200 GET "$WS/v1/workspaces/us-ops/inbox" "$MEMBER" | field "['items'][0]['title']")" == "Approve the refund" ]] || { echo "FAIL: inbox" >&2; exit 1; }
# the web shell is served by the image, with its CSP, without a token
curl -sf --noproxy '*' -D .local/shell.headers -o .local/shell.html "$WS/w/us-ops"
grep -q '<div id="root">' .local/shell.html && grep -qi "content-security-policy: default-src 'self'" .local/shell.headers || { echo "FAIL: web shell" >&2; exit 1; }
ASSET="$(grep -o '/assets/[^"]*\.js' .local/shell.html | head -1)"
curl -sf --noproxy '*' -o /dev/null "$WS$ASSET" || { echo "FAIL: shell asset $ASSET" >&2; exit 1; }
curl -sf --noproxy '*' http://127.0.0.1:9566/metrics | grep -q 'idp_workspace' || { echo "FAIL: metrics" >&2; exit 1; }

if [[ "$HOST" == 1 ]]; then
  # an agent on the HOST, approved (four eyes), then an app whose action runs it
  call 201 POST "$HOSTURL/api/v1/agents" "$H_ADMIN" '{"name":"order-assistant","displayName":"Order assistant","definition":{"instructions":"Answer questions about orders.","model":{"provider":"local","model":"fake-1"},"tools":{},"confirmation":{"default":"auto"}}}' >/dev/null
  REL="$(call 201 POST "$HOSTURL/api/v1/agents/order-assistant/releases" "$H_ADMIN" | field "['releaseId']")"
  call 200 POST "$HOSTURL/api/v1/releases/$REL/approve" "$H_APPROVER" >/dev/null
  call 201 POST "$WS/api/v1/apps" "$ADMIN" '{"schemaVersion":"idp.workspace.app/v1","name":"orders","version":"1.0.0","title":"Orders","publisher":"IDP","capabilities":[],"actions":[{"name":"order.review","title":"Ask the order assistant","target":{"kind":"host.run","agent":"order-assistant"},"input":{"type":"object","required":["message"],"properties":{"message":{"type":"string"}},"additionalProperties":false}}]}' >/dev/null
  call 201 PUT "$WS/api/v1/workspaces/us-ops/installations/orders" "$ADMIN" '{"version":"1.0.0"}' >/dev/null
  call 200 PUT "$WS/v1/workspaces/us-ops/context" "$MEMBER" '{"entities":[{"type":"order","id":"1001"}]}' >/dev/null
  INV="$(call 201 POST "$WS/v1/workspaces/us-ops/actions/invoke" "$MEMBER" '{"app":"orders","action":"order.review","input":{"message":"Why is order 1001 late?"}}')"
  RUN="$(echo "$INV" | field "['result']['id']")"
  [[ "$RUN" == ws_* ]] || { echo "FAIL: no run id in $INV" >&2; exit 1; }
  RESULT=""
  for _ in $(seq 1 120); do
    RESULT="$(call 200 GET "$WS/v1/workspaces/us-ops/inbox?kind=run" "$MEMBER" | python3 -c "
import json,sys
for it in json.load(sys.stdin)['items']:
    if it['externalRef']=='run:$RUN' and not it['title'].endswith('running'): print(it['title']+' | '+it.get('summary',''))")"
    [[ -n "$RESULT" ]] && break; sleep 0.5
  done
  [[ "$RESULT" == *"— finished |"* ]] || { "${C[@]}" logs --tail 30 workspace host host-worker; echo "FAIL: run result not in the inbox: '$RESULT'" >&2; exit 1; }
  STATUS_TOKEN="$(cat .local/host.key)"; [[ "$OIDC" == 1 ]] && STATUS_TOKEN="$H_ADMIN"
  STATUS="$(call 200 GET "$HOSTURL/v1/runs/$RUN" "$STATUS_TOKEN" | field "['status']")"
  [[ "$STATUS" == COMPLETED ]] || { echo "FAIL: HOST run $RUN is $STATUS" >&2; exit 1; }
  echo "PASS: host.run through the workspace: run $RUN COMPLETED on idp-host; inbox: $RESULT"
fi
if [[ "${BROWSER:-0}" == 1 ]]; then
  # a third-party app on its own origin (loopback http is allowed for entries)
  python3 -m http.server 9700 --bind 127.0.0.1 --directory tests/browser >/dev/null 2>&1 & APPSRV=$!
  trap 'kill $APPSRV 2>/dev/null; cleanup' EXIT
  call 201 POST "$WS/api/v1/apps" "$ADMIN" '{"schemaVersion":"idp.workspace.app/v1","name":"demo","version":"1.0.0","title":"Demo","publisher":"IDP","entry":{"url":"http://127.0.0.1:9700/demo-app.html"},"capabilities":["context.read","actions.invoke","ui.notify"],"actions":[{"name":"demo.ping","title":"Ping","target":{"kind":"webhook","url":"http://127.0.0.1:9701/hook"},"input":{"type":"object"}}]}' >/dev/null
  call 201 PUT "$WS/api/v1/workspaces/us-ops/installations/demo" "$ADMIN" '{"version":"1.0.0"}' >/dev/null
  call 200 PUT "$WS/v1/workspaces/us-ops/context" "$MEMBER" '{"entities":[{"type":"order","id":"1001"}]}' >/dev/null
  if [[ "$OIDC" == 1 ]]; then
    call 201 POST "$WS/api/v1/apps" "$ADMIN" '{"schemaVersion":"idp.workspace.app/v1","name":"desk","version":"1.0.0","title":"Desk","publisher":"IDP","entry":{"url":"http://127.0.0.1:9700/token-app.html"},"capabilities":["identity.token"],"actions":[]}' >/dev/null
    call 201 PUT "$WS/api/v1/workspaces/us-ops/installations/desk" "$ADMIN" '{"version":"1.0.0"}' >/dev/null
    WS_URL="$WS" PRODUCER_TOKEN="$PRODUCER" KC_USER=bob KC_PASSWORD="$(secret_of bob)" node tests/browser/oidc.smoke.mjs
  else
    WS_URL="$WS" MEMBER_TOKEN="$MEMBER" PRODUCER_TOKEN="$PRODUCER" ADMIN_TOKEN="$ADMIN" node tests/browser/shell.smoke.mjs
  fi
fi
echo "PASS: workspace image e2e (tenant $TENANT$([[ "$HOST" == 1 ]] && echo ', with idp-host'))"
