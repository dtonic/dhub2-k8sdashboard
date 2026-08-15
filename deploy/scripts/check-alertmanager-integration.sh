#!/usr/bin/env sh
set -eu

ROOT=$(CDPATH= cd -- "$(dirname "$0")/../.." && pwd)
BASE=$(realpath "${TMPDIR:-/tmp}")
TMP=$(mktemp -d "$BASE/issue1-alertmanager.XXXXXX")
TOKEN=$(basename "$TMP" | tr '[:upper:]' '[:lower:]')
CONTAINER="issue1-alertmanager-${TOKEN##*.}"
REDIS_CONTAINER="issue1-alertmanager-redis-${TOKEN##*.}"
NGINX_MAIN="issue1-alertmanager-nginx-main-${TOKEN##*.}"
NGINX_OUTAGE="issue1-alertmanager-nginx-outage-${TOKEN##*.}"
PROXY_CONTAINER="issue1-alertmanager-proxy-${TOKEN##*.}"
AUTH_MAIN="issue1-alertmanager-auth-main-${TOKEN##*.}"
AUTH_OUTAGE="issue1-alertmanager-auth-outage-${TOKEN##*.}"
NETWORK="issue1-alertmanager-${TOKEN##*.}"
IMAGE='quay.io/prometheus/alertmanager@sha256:27c475db5fb156cab31d5c18a4251ac7ed567746a2483ff264516437a39b15ba'
REDIS_IMAGE='redis:8.2.6-alpine@sha256:ea5a07305d6c66f99df5a5ff8d9659e8f6cb598e6e586dc8dd92b7fcd915746e'
NGINX_IMAGE='nginxinc/nginx-unprivileged:1.30.4-alpine@sha256:44e36330f74d4f3a1d4e222acca9e23b401fb87811a7597024502bb759c4dd49'
PYTHON_IMAGE='python@sha256:05b2b8b732ecd268fee8727a369f936f022d1321b59befd13c30ede22769dcdc'
API_RUNTIME_IMAGE='gcr.io/distroless/static-debian12:nonroot@sha256:1b7b9f0f0e0a1d2155f531db587cc48ec26aaf97ab64364225f5bf18a054e66a'
CONTAINER_ID=''
REDIS_ID=''
NGINX_MAIN_ID=''
NGINX_OUTAGE_ID=''
PROXY_CONTAINER_ID=''
AUTH_MAIN_ID=''
AUTH_OUTAGE_ID=''
NETWORK_ID=''
PROXY_PID=''
AUTH_MAIN_PID=''
AUTH_OUTAGE_PID=''
IMAGE_WAS_PRESENT=false
REDIS_WAS_PRESENT=false
NGINX_WAS_PRESENT=false
PYTHON_WAS_PRESENT=false
API_RUNTIME_WAS_PRESENT=false
docker image inspect "$IMAGE" >/dev/null 2>&1 && IMAGE_WAS_PRESENT=true
docker image inspect "$REDIS_IMAGE" >/dev/null 2>&1 && REDIS_WAS_PRESENT=true
docker image inspect "$NGINX_IMAGE" >/dev/null 2>&1 && NGINX_WAS_PRESENT=true
docker image inspect "$PYTHON_IMAGE" >/dev/null 2>&1 && PYTHON_WAS_PRESENT=true
docker image inspect "$API_RUNTIME_IMAGE" >/dev/null 2>&1 && API_RUNTIME_WAS_PRESENT=true

cleanup() {
  status=$?
  if [ -n "$PROXY_PID" ]; then
    kill "$PROXY_PID" >/dev/null 2>&1 || true
    wait "$PROXY_PID" >/dev/null 2>&1 || true
  fi
  for pair in "$NGINX_MAIN:$NGINX_MAIN_ID" "$NGINX_OUTAGE:$NGINX_OUTAGE_ID" "$AUTH_MAIN:$AUTH_MAIN_ID" "$AUTH_OUTAGE:$AUTH_OUTAGE_ID" "$PROXY_CONTAINER:$PROXY_CONTAINER_ID" "$REDIS_CONTAINER:$REDIS_ID"; do
    name=${pair%%:*}; id=${pair#*:}
    if [ -n "$id" ] && [ "$(docker inspect -f '{{.Id}}' "$name" 2>/dev/null || true)" = "$id" ]; then docker rm -f "$name" >/dev/null; fi
  done
  if [ -n "$CONTAINER_ID" ] && [ "$(docker inspect -f '{{.Id}}' "$CONTAINER" 2>/dev/null || true)" = "$CONTAINER_ID" ]; then
    docker rm -f "$CONTAINER" >/dev/null
  fi
  if [ -n "$NETWORK_ID" ] && [ "$(docker network inspect -f '{{.Id}}' "$NETWORK" 2>/dev/null || true)" = "$NETWORK_ID" ]; then
    docker network rm "$NETWORK" >/dev/null 2>&1 || true
  fi
  if [ "$IMAGE_WAS_PRESENT" = false ]; then
    docker image rm "$IMAGE" >/dev/null 2>&1 || true
  fi
  if [ "$REDIS_WAS_PRESENT" = false ]; then docker image rm "$REDIS_IMAGE" >/dev/null 2>&1 || true; fi
  if [ "$NGINX_WAS_PRESENT" = false ]; then docker image rm "$NGINX_IMAGE" >/dev/null 2>&1 || true; fi
  if [ "$PYTHON_WAS_PRESENT" = false ]; then docker image rm "$PYTHON_IMAGE" >/dev/null 2>&1 || true; fi
  if [ "$API_RUNTIME_WAS_PRESENT" = false ]; then docker image rm "$API_RUNTIME_IMAGE" >/dev/null 2>&1 || true; fi
  resolved=$(realpath "$TMP" 2>/dev/null || true)
  case "$resolved" in
    "$BASE"/issue1-alertmanager.*)
      for immutable in runtime-bin proxy-secrets api-secrets; do
        [ ! -d "$resolved/$immutable" ] || chmod 0700 "$resolved/$immutable"
      done
      [ ! -d "$resolved" ] || rm -rf -- "$resolved"
      ;;
    *) echo "refusing to clean unexpected Alertmanager temp path: $resolved" >&2; status=1 ;;
  esac
  exit "$status"
}
trap cleanup EXIT HUP INT TERM

openssl req -x509 -newkey rsa:2048 -nodes -days 1 -subj /CN=issue1-alertmanager-ca \
  -keyout "$TMP/ca.key" -out "$TMP/ca.crt" >/dev/null 2>&1
openssl req -newkey rsa:2048 -nodes -subj /CN=alertmanager.test \
  -keyout "$TMP/server.key" -out "$TMP/server.csr" >/dev/null 2>&1
printf '%s\n' 'subjectAltName=DNS:alertmanager.test' 'extendedKeyUsage=serverAuth' > "$TMP/server.ext"
openssl x509 -req -days 1 -in "$TMP/server.csr" -CA "$TMP/ca.crt" -CAkey "$TMP/ca.key" -CAcreateserial \
  -extfile "$TMP/server.ext" -out "$TMP/server.crt" >/dev/null 2>&1
openssl req -newkey rsa:2048 -nodes -subj /CN=dashboard-api \
  -keyout "$TMP/client.key" -out "$TMP/client.csr" >/dev/null 2>&1
printf '%s\n' 'extendedKeyUsage=clientAuth' > "$TMP/client.ext"
openssl x509 -req -days 1 -in "$TMP/client.csr" -CA "$TMP/ca.crt" -CAkey "$TMP/ca.key" -CAcreateserial \
  -extfile "$TMP/client.ext" -out "$TMP/client.crt" >/dev/null 2>&1
openssl req -newkey rsa:2048 -nodes -subj /CN=wrong-client-role \
  -keyout "$TMP/wrong-client.key" -out "$TMP/wrong-client.csr" >/dev/null 2>&1
printf '%s\n' 'extendedKeyUsage=serverAuth' > "$TMP/wrong-client.ext"
openssl x509 -req -days 1 -in "$TMP/wrong-client.csr" -CA "$TMP/ca.crt" -CAkey "$TMP/ca.key" -CAcreateserial \
  -extfile "$TMP/wrong-client.ext" -out "$TMP/wrong-client.crt" >/dev/null 2>&1
openssl req -x509 -newkey rsa:2048 -nodes -days 1 -subj /CN=issue1-wrong-ca \
  -keyout "$TMP/wrong-ca.key" -out "$TMP/wrong-ca.crt" >/dev/null 2>&1
openssl rand -hex -out "$TMP/token" 32
chmod 0600 "$TMP"/*.key "$TMP/token"
BROWSER_TOKEN=$(cat "$TMP/token")

timeout 60s docker pull "$IMAGE" >/dev/null
NETWORK_ID=$(docker network create "$NETWORK")
CONTAINER_ID=$(docker run -d --name "$CONTAINER" --network "$NETWORK" --network-alias alertmanager -p 127.0.0.1::9093 \
  -v "$ROOT/deploy/alertmanager/alertmanager.yml:/etc/alertmanager/alertmanager.yml:ro" \
  "$IMAGE" --config.file=/etc/alertmanager/alertmanager.yml --storage.path=/alertmanager)
ADMIN_PORT=$(docker port "$CONTAINER" 9093/tcp | awk -F: 'NR==1 {print $NF}')
case "$ADMIN_PORT" in ''|*[!0-9]*) echo "invalid Alertmanager fixture port" >&2; exit 1;; esac
ADMIN_URL="http://127.0.0.1:$ADMIN_PORT"
ready=false
for _ in $(seq 1 100); do
  if curl --connect-timeout 1 --max-time 2 --silent --fail "$ADMIN_URL/-/ready" >/dev/null 2>&1; then ready=true; break; fi
  sleep 0.1
done
[ "$ready" = true ] || { docker logs "$CONTAINER" >&2; exit 1; }

python3 "$ROOT/deploy/alertmanager/proxy.py" --upstream "$ADMIN_URL" \
  --cert "$TMP/server.crt" --key "$TMP/server.key" --token-file "$TMP/token" \
  --client-ca "$TMP/ca.crt" \
  --stats "$TMP/stats.json" --port-file "$TMP/proxy.port" >"$TMP/proxy.log" 2>&1 &
PROXY_PID=$!
for _ in $(seq 1 100); do [ -s "$TMP/proxy.port" ] && break; sleep 0.05; done
[ -s "$TMP/proxy.port" ] || { cat "$TMP/proxy.log" >&2; exit 1; }
PROXY_PORT=$(cat "$TMP/proxy.port")
case "$PROXY_PORT" in ''|*[!0-9]*) echo "invalid proxy port" >&2; exit 1;; esac

cd "$ROOT/apps/api"
ALERTMANAGER_ITEST_URL="https://127.0.0.1:$PROXY_PORT" \
ALERTMANAGER_ITEST_ADMIN_URL="$ADMIN_URL" \
ALERTMANAGER_ITEST_TOKEN_FILE="$TMP/token" \
ALERTMANAGER_ITEST_CA_FILE="$TMP/ca.crt" \
ALERTMANAGER_ITEST_WRONG_CA_FILE="$TMP/wrong-ca.crt" \
ALERTMANAGER_ITEST_CLIENT_CERT_FILE="$TMP/client.crt" \
ALERTMANAGER_ITEST_CLIENT_KEY_FILE="$TMP/client.key" \
ALERTMANAGER_ITEST_WRONG_CLIENT_CERT_FILE="$TMP/wrong-client.crt" \
ALERTMANAGER_ITEST_WRONG_CLIENT_KEY_FILE="$TMP/wrong-client.key" \
ALERTMANAGER_ITEST_STATS_FILE="$TMP/stats.json" \
  timeout 120s go test -tags integration -count=1 -v -timeout 2m ./internal/datasource/alertmanager \
    -run TestActualAlertmanagerPrivateCABearerScopeAndFailures

[ "$(docker inspect -f '{{.Config.Image}}' "$CONTAINER")" = "$IMAGE" ]
python3 - "$TMP/stats.json" <<'PY'
import json,sys
requests=json.load(open(sys.argv[1],encoding="utf-8"))["requests"]
assert requests and all(item["method"]=="GET" for item in requests),requests
assert not any("Bearer" in json.dumps(item) or "token" in json.dumps(item).lower() for item in requests),requests
print(f"actual Alertmanager fixture passed: proxy GETs={len(requests)}")
PY

free_port() {
  python3 - <<'PY'
import socket
s=socket.socket();s.bind(("127.0.0.1",0));print(s.getsockname()[1]);s.close()
PY
}

timeout 60s docker pull "$REDIS_IMAGE" >/dev/null
REDIS_ID=$(docker run -d --name "$REDIS_CONTAINER" --network "$NETWORK" --network-alias redis "$REDIS_IMAGE")
for _ in $(seq 1 100); do docker exec "$REDIS_CONTAINER" redis-cli ping 2>/dev/null | grep -q PONG && break; sleep 0.05; done
docker exec "$REDIS_CONTAINER" redis-cli ping | grep -q PONG
echo 'browser fixture: redis ready'

SESSION_KEY=$(openssl rand -base64 32 | tr '+/' '-_' | tr -d '=\n')
MAIN_PORT=$(free_port); OUTAGE_PORT=$(free_port)
mkdir "$TMP/runtime-bin" "$TMP/proxy-secrets" "$TMP/api-secrets" "$TMP/proxy-state" "$TMP/auth-main-state" "$TMP/auth-outage-state" "$TMP/nginx"
cp "$TMP/server.crt" "$TMP/server.key" "$TMP/ca.crt" "$TMP/token" "$TMP/proxy-secrets/"
cp "$TMP/ca.crt" "$TMP/client.crt" "$TMP/client.key" "$TMP/token" "$TMP/api-secrets/"
cd "$ROOT/apps/api"
CGO_ENABLED=0 timeout 120s go build -tags e2efixture -trimpath -o "$TMP/runtime-bin/authfixture" ./cmd/authfixture
echo 'browser fixture: auth binary built'
chmod 0440 "$TMP/proxy-secrets"/* "$TMP/api-secrets"/*
chmod 0555 "$TMP/runtime-bin/authfixture"
SECRET_HASH=$(sha256sum "$TMP/proxy-secrets"/* "$TMP/api-secrets"/* | cut -d ' ' -f1)
touch "$TMP/proxy-state/stats.json" "$TMP/proxy-state/port" \
  "$TMP/auth-main-state/ready.json" "$TMP/auth-main-state/tls.crt" "$TMP/auth-main-state/tls.key" \
  "$TMP/auth-outage-state/ready.json" "$TMP/auth-outage-state/tls.crt" "$TMP/auth-outage-state/tls.key"
chmod 0664 "$TMP/proxy-state/stats.json" "$TMP/proxy-state/port" \
  "$TMP/auth-main-state/ready.json" "$TMP/auth-outage-state/ready.json"
chmod 0660 "$TMP/auth-main-state/tls.crt" "$TMP/auth-main-state/tls.key" \
  "$TMP/auth-outage-state/tls.crt" "$TMP/auth-outage-state/tls.key"
timeout 60s docker pull "$PYTHON_IMAGE" >/dev/null
timeout 120s docker pull "$API_RUNTIME_IMAGE" >/dev/null
echo 'browser fixture: runtime images ready'
HOST_UID=$(id -u)
case "$HOST_UID" in ''|*[!0-9]*) echo 'invalid fixture host uid' >&2; exit 1;; esac
docker run --rm --entrypoint /bin/sh \
  -v "$TMP/runtime-bin:/runtime-bin" -v "$TMP/proxy-secrets:/proxy-secrets" -v "$TMP/api-secrets:/api-secrets" -v "$TMP/proxy-state:/proxy-state" \
  -v "$TMP/auth-main-state:/auth-main-state" -v "$TMP/auth-outage-state:/auth-outage-state" "$PYTHON_IMAGE" -c \
  "chown 65532:65532 /runtime-bin/* /proxy-secrets/* /api-secrets/* /proxy-state/* /auth-main-state/* /auth-outage-state/* && \
   chown $HOST_UID:65532 /runtime-bin /proxy-secrets /api-secrets /proxy-state /auth-main-state /auth-outage-state && \
   chmod 0550 /runtime-bin /proxy-secrets /api-secrets && chmod 0770 /proxy-state /auth-main-state /auth-outage-state" >/dev/null
PROXY_CONTAINER_ID=$(docker run -d --name "$PROXY_CONTAINER" --network "$NETWORK" --network-alias proxy --user 65532:65532 \
  -v "$ROOT/deploy/alertmanager/proxy.py:/proxy.py:ro" -v "$TMP/proxy-secrets:/secrets:ro" -v "$TMP/proxy-state:/state" "$PYTHON_IMAGE" python3 /proxy.py --container-network --upstream http://alertmanager:9093 \
  --cert /secrets/server.crt --key /secrets/server.key --token-file /secrets/token --client-ca /secrets/ca.crt \
  --stats /state/stats.json --port-file /state/port)
for _ in $(seq 1 100); do [ -s "$TMP/proxy-state/port" ] && break; sleep 0.05; done
[ "$(cat "$TMP/proxy-state/port")" = 9443 ] || { docker logs "$PROXY_CONTAINER" >&2; exit 1; }
[ "$(docker inspect -f '{{range .Mounts}}{{if eq .Destination "/secrets"}}{{.RW}}{{end}}{{end}}' "$PROXY_CONTAINER")" = false ]
if docker exec "$PROXY_CONTAINER" python3 -c 'open("/secrets/token","wb").write(b"mutated")' >/dev/null 2>&1; then
  echo 'read-only credential mount accepted mutation' >&2; exit 1
fi
[ "$(docker exec "$PROXY_CONTAINER" python3 -c 'import os;print(os.path.exists("/secrets/client.key"))')" = False ]
echo 'browser fixture: private proxy ready'
AUTH_MAIN_ID=$(docker run -d --name "$AUTH_MAIN" --hostname auth-main --network "$NETWORK" --network-alias auth-main --user 65532:65532 \
  --entrypoint /runtime/authfixture -v "$TMP/runtime-bin:/runtime:ro" -v "$TMP/api-secrets:/secrets:ro" -v "$TMP/auth-main-state:/state" -v "$ROOT/apps/web/dist:/web:ro" "$API_RUNTIME_IMAGE" \
  -dist /web -redis redis:6379 -key "$SESSION_KEY" -backend-addr "0.0.0.0:9444" \
  -public-origin "https://127.0.0.1:$MAIN_PORT" -ready-file /state/ready.json -cert-file /state/tls.crt -key-file /state/tls.key \
  -alertmanager-url https://proxy:9443/am -alertmanager-public-url https://alerts.public.test/am \
  -alertmanager-token-file /secrets/token -alertmanager-ca-file /secrets/ca.crt -alertmanager-client-cert-file /secrets/client.crt \
  -alertmanager-client-key-file /secrets/client.key -alertmanager-server-name alertmanager.test)
AUTH_OUTAGE_ID=$(docker run -d --name "$AUTH_OUTAGE" --hostname auth-outage --network "$NETWORK" --network-alias auth-outage --user 65532:65532 \
  --entrypoint /runtime/authfixture -v "$TMP/runtime-bin:/runtime:ro" -v "$TMP/api-secrets:/secrets:ro" -v "$TMP/auth-outage-state:/state" -v "$ROOT/apps/web/dist:/web:ro" "$API_RUNTIME_IMAGE" \
  -dist /web -redis redis:6379 -key "$SESSION_KEY" -backend-addr "0.0.0.0:9444" \
  -public-origin "https://127.0.0.1:$OUTAGE_PORT" -ready-file /state/ready.json -cert-file /state/tls.crt -key-file /state/tls.key \
  -alertmanager-url https://proxy:9443/outage -alertmanager-public-url https://alerts.public.test/am \
  -alertmanager-token-file /secrets/token -alertmanager-ca-file /secrets/ca.crt -alertmanager-client-cert-file /secrets/client.crt \
  -alertmanager-client-key-file /secrets/client.key -alertmanager-server-name alertmanager.test)
[ "$(docker inspect -f '{{.Config.Image}} {{.Config.User}} {{json .Config.Entrypoint}}' "$AUTH_MAIN")" = "$API_RUNTIME_IMAGE 65532:65532 [\"/runtime/authfixture\"]" ]
[ "$(docker inspect -f '{{.Config.Image}} {{.Config.User}} {{json .Config.Entrypoint}}' "$AUTH_OUTAGE")" = "$API_RUNTIME_IMAGE 65532:65532 [\"/runtime/authfixture\"]" ]
for auth_container in "$AUTH_MAIN" "$AUTH_OUTAGE"; do
  case "$(docker inspect -f '{{json .HostConfig.PortBindings}}' "$auth_container")" in null|'{}') ;; *) echo 'auth fixture published a host port' >&2; exit 1;; esac
  [ -z "$(docker port "$auth_container")" ] || { echo 'auth fixture exposed a published port' >&2; exit 1; }
done
NETWORK_ID=$(docker network inspect -f '{{.Id}}' "$NETWORK")
[ "$(docker inspect -f "{{with index .NetworkSettings.Networks \"$NETWORK\"}}{{.NetworkID}}{{end}}" "$AUTH_MAIN")" = "$NETWORK_ID" ]
[ "$(docker inspect -f "{{with index .NetworkSettings.Networks \"$NETWORK\"}}{{.NetworkID}}{{end}}" "$AUTH_OUTAGE")" = "$NETWORK_ID" ]
docker inspect -f "{{with index .NetworkSettings.Networks \"$NETWORK\"}}{{json .Aliases}}{{end}}" "$AUTH_MAIN" | grep -Fq '"auth-main"'
docker inspect -f "{{with index .NetworkSettings.Networks \"$NETWORK\"}}{{json .Aliases}}{{end}}" "$AUTH_OUTAGE" | grep -Fq '"auth-outage"'
[ "$(stat -c '%a' "$TMP/runtime-bin/authfixture")" = 555 ]
[ "$(stat -c '%a' "$TMP/api-secrets/token")" = 440 ]
[ "$(stat -c '%a:%g' "$TMP/runtime-bin")" = 550:65532 ]
[ "$(docker inspect -f '{{range .Mounts}}{{if eq .Destination "/secrets"}}{{.RW}}{{end}}{{end}}' "$AUTH_MAIN")" = false ]
[ "$(docker inspect -f '{{range .Mounts}}{{if eq .Destination "/secrets"}}{{.RW}}{{end}}{{end}}' "$AUTH_OUTAGE")" = false ]
API_HAS_SERVER_KEY=$(docker run --rm --volumes-from "$AUTH_MAIN":ro "$PYTHON_IMAGE" python3 -c 'import os;print(os.path.exists("/secrets/server.key"))')
[ "$API_HAS_SERVER_KEY" = False ]
[ ! -e "$ROOT/apps/api/authfixture" ]
echo 'browser fixture: auth runtimes verified'
for _ in $(seq 1 300); do [ -s "$TMP/auth-main-state/ready.json" ] && [ -s "$TMP/auth-outage-state/ready.json" ] && break; sleep 0.1; done
if [ ! -s "$TMP/auth-main-state/ready.json" ] || [ ! -s "$TMP/auth-outage-state/ready.json" ]; then docker logs "$AUTH_MAIN" >&2; docker logs "$AUTH_OUTAGE" >&2; exit 1; fi
echo 'browser fixture: auth sessions ready'
MAIN_UPSTREAM=$(python3 -c 'import json,sys,urllib.parse;print(urllib.parse.urlsplit(json.load(open(sys.argv[1]))["fixtureURL"]).port)' "$TMP/auth-main-state/ready.json")
OUTAGE_UPSTREAM=$(python3 -c 'import json,sys,urllib.parse;print(urllib.parse.urlsplit(json.load(open(sys.argv[1]))["fixtureURL"]).port)' "$TMP/auth-outage-state/ready.json")
[ "$MAIN_UPSTREAM" = 9444 ]
[ "$OUTAGE_UPSTREAM" = 9444 ]
sed -e "s/__UPSTREAM_HOST__/auth-main/g" -e "s/__UPSTREAM_PORT__/9444/g" "$ROOT/deploy/alertmanager/nginx-auth.conf" > "$TMP/nginx/main.conf"
sed -e "s/__UPSTREAM_HOST__/auth-outage/g" -e "s/__UPSTREAM_PORT__/9444/g" "$ROOT/deploy/alertmanager/nginx-auth.conf" > "$TMP/nginx/outage.conf"
docker run --rm --entrypoint /bin/sh -v "$TMP/auth-main-state:/auth-main" -v "$TMP/auth-outage-state:/auth-outage" -v "$TMP/nginx:/nginx" "$PYTHON_IMAGE" -c \
  'chmod 0444 /auth-main/tls.crt /auth-main/tls.key /auth-outage/tls.crt /auth-outage/tls.key /nginx/main.conf /nginx/outage.conf' >/dev/null

timeout 60s docker pull "$NGINX_IMAGE" >/dev/null
NGINX_MAIN_ID=$(docker run -d --name "$NGINX_MAIN" --network "$NETWORK" -p "127.0.0.1:$MAIN_PORT:8443" \
  -v "$TMP/nginx/main.conf:/etc/nginx/nginx.conf:ro" -v "$TMP/auth-main-state/tls.crt:/fixture/tls.crt:ro" -v "$TMP/auth-main-state/tls.key:/fixture/tls.key:ro" "$NGINX_IMAGE")
NGINX_OUTAGE_ID=$(docker run -d --name "$NGINX_OUTAGE" --network "$NETWORK" -p "127.0.0.1:$OUTAGE_PORT:8443" \
  -v "$TMP/nginx/outage.conf:/etc/nginx/nginx.conf:ro" -v "$TMP/auth-outage-state/tls.crt:/fixture/tls.crt:ro" -v "$TMP/auth-outage-state/tls.key:/fixture/tls.key:ro" "$NGINX_IMAGE")
for origin in "https://127.0.0.1:$MAIN_PORT" "https://127.0.0.1:$OUTAGE_PORT"; do
  ready=false
  for _ in $(seq 1 30); do if curl --connect-timeout 1 --max-time 2 --insecure --silent --fail "$origin/readyz" >/dev/null 2>&1; then ready=true; break; fi; sleep 0.1; done
  [ "$ready" = true ] || { docker logs "$NGINX_MAIN" >&2; docker logs "$NGINX_OUTAGE" >&2; exit 1; }
done
echo 'browser fixture: nginx ready'

cd "$ROOT/apps/web"
if ! ALERTMANAGER_BROWSER_URL="https://127.0.0.1:$MAIN_PORT" ALERTMANAGER_BROWSER_OUTAGE_URL="https://127.0.0.1:$OUTAGE_PORT" ALERTMANAGER_BROWSER_TOKEN="$BROWSER_TOKEN" \
  WSLENV="${WSLENV:+$WSLENV:}ALERTMANAGER_BROWSER_URL:ALERTMANAGER_BROWSER_OUTAGE_URL:ALERTMANAGER_BROWSER_TOKEN" \
  timeout 120s npx playwright test --config playwright.alertmanager.config.ts; then
  python3 - "$TMP/proxy-state/stats.json" <<'PY'
import collections,json,sys
requests=json.load(open(sys.argv[1],encoding="utf-8"))["requests"]
print("browser proxy requests:",dict(collections.Counter((r["method"],r["path"]) for r in requests)))
PY
  docker logs "$AUTH_MAIN" >&2
  docker logs "$AUTH_OUTAGE" >&2
  exit 1
fi
python3 - "$TMP/proxy-state/stats.json" "$BROWSER_TOKEN" <<'PY'
import collections,json,sys
payload=open(sys.argv[1],encoding="utf-8").read()
requests=json.loads(payload)["requests"]
expected={('GET','/am/api/v2/alerts'):2,('GET','/outage/api/v2/alerts'):6}
def validate(items):
    counts=dict(collections.Counter((item["method"],item["path"]) for item in items))
    if len(items) != 8 or counts != expected or any(item["method"] != "GET" for item in items):
        raise ValueError(counts)
validate(requests)
if sys.argv[2] in payload: raise SystemExit("browser proxy stats leaked bearer token")
for mutant in (requests[:-1], requests + [dict(requests[0])]):
    try: validate(mutant)
    except ValueError: pass
    else: raise SystemExit("browser proxy count mutation was accepted")
print("browser Alertmanager fixture passed: GET-only main=2 outage=6 total=8")
PY
FINAL_SECRET_HASH=$(docker run --rm --entrypoint /bin/sh -v "$TMP/proxy-secrets:/proxy-secrets:ro" -v "$TMP/api-secrets:/api-secrets:ro" "$PYTHON_IMAGE" -c \
  "sha256sum /proxy-secrets/* /api-secrets/* | cut -d ' ' -f1")
[ "$FINAL_SECRET_HASH" = "$SECRET_HASH" ]
