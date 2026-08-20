#!/usr/bin/env bash
# deploy.sh — 단일 클러스터 시험 배포 도구 (Issue #26)
#
# 사용법:
#   ./deploy.sh <cluster-context> [namespace ...]
#
#   <cluster-context>  kubeconfig 컨텍스트 이름 (예: lnode)
#   [namespace ...]    대시보드에 노출할 namespace 목록 (0개 이상).
#                      생략하면 전체 namespace(SCOPE_NAMESPACES="*")를 노출합니다.
#
# 예시:
#   ./deploy.sh lnode dhub2          # dhub2만 노출
#   ./deploy.sh lnode dhub2 dhub3    # 두 개 노출
#   ./deploy.sh lnode                # 전체 노출
#
# 환경변수 (선택):
#   REGISTRY           이미지 push 대상 registry/project (기본: registry.hub.dtonic.io/lnode
#                      — Harbor라 <registry>/<project> 2단계 경로가 필요합니다)
#   RELEASE_NAME       helm release 이름          (기본: dashboard)
#   RELEASE_NAMESPACE  설치 namespace             (기본: observability)
#   IMAGE_TAG          이미지 tag                 (기본: dev-<git short sha>)
#   SKIP_BUILD         1이면 빌드/push 생략하고 기존 tag로 helm만 실행
#   AUTO_DATASOURCES   1(기본)이면 클러스터 안의 GreptimeDB/Quickwit을 자동 발견해
#                      실데이터로 연결합니다. 0이면 demo 데이터로 배포합니다.
#   GREPTIME_URL_OVERRIDE / GREPTIME_DB_OVERRIDE / QUICKWIT_URL_OVERRIDE /
#   QUICKWIT_INDEX_OVERRIDE  자동 발견 대신 수동 지정합니다.
#   ROUTE_HOST         OpenShift/OKD에서 UI를 노출할 Route 호스트명
#                      (예: k8sdashboard.apps.okd.dtonic.io). 비우면 기존
#                      <release>-ui Route의 host를 재사용합니다. OIDC 모드에서는
#                      HTTPS 공개 origin이 필수라 둘 다 없으면 배포를 중단합니다.
#   AUTH_MODE          기본 oidc — Keycloak OIDC + 브라우저 세션(ADR 0011)으로
#                      배포합니다. none이면 인증 없는 개방 배포(개발·데모 전용)이며
#                      사내망 누구나 접근 가능합니다.
#   OIDC_ISSUER        발급자 HTTPS URL. 비우면 클러스터의 keycloak Route를 자동
#                      발견해 https://<host>/realms/$OIDC_REALM 을 사용합니다.
#   OIDC_REALM         issuer 자동 발견 시 realm 이름   (기본: dhub2)
#   OIDC_CLIENT_ID     public PKCE client id            (기본: k8s-dashboard)
#   OIDC_AUDIENCE      API Bearer 토큰 audience         (기본: OIDC_CLIENT_ID)
#   OIDC_CLIENT_SECRET confidential client일 때만 지정. Secret으로만 주입됩니다.
#
#   IdP 사전 조건(한 번만): 해당 realm에 OIDC_CLIENT_ID public client가 있어야 하고
#   (redirect URI = https://<ROUTE_HOST>/api/v1/auth/callback, PKCE S256),
#   flat "roles" claim 매퍼(ID/access token)와 client role(platform.admin 등)이
#   로그인할 계정에 부여되어 있어야 합니다. 자세한 명세는 deploy/README.md 참고.
#
# 동작 개요:
#   chart의 NetworkPolicy는 항상 켜져 있습니다(schema 강제). 이 스크립트는
#   대상 클러스터에서 Kubernetes API endpoint(IP/port)와 DNS 위치를 조회해
#   values overlay로 주입하고, chart values로 표현할 수 없는 부분
#   (OKD의 apiserver 6443 포트, openshift-dns 5353 포트)은 추가
#   NetworkPolicy 하나로 보완합니다. OpenShift/OKD에서는 chart가 고정한
#   runAsUser 때문에 nonroot-v2 SCC RoleBinding도 함께 만듭니다.
#
# WSL(Ubuntu) bash 전용입니다. docker / helm / kubectl이 필요합니다.
set -euo pipefail

die() { echo "오류: $*" >&2; exit 1; }

[ $# -ge 1 ] || die "사용법: ./deploy.sh <cluster-context> [namespace ...]"
CLUSTER="$1"; shift

if [ $# -gt 0 ]; then
  SCOPE="$(IFS=,; echo "$*")"
else
  SCOPE="*"
fi

REGISTRY="${REGISTRY:-registry.hub.dtonic.io/lnode}"
RELEASE_NAME="${RELEASE_NAME:-dashboard}"
RELEASE_NAMESPACE="${RELEASE_NAMESPACE:-observability}"
SKIP_BUILD="${SKIP_BUILD:-0}"
ROUTE_HOST="${ROUTE_HOST:-}"
AUTH_MODE="${AUTH_MODE:-oidc}"
OIDC_REALM="${OIDC_REALM:-dhub2}"
OIDC_CLIENT_ID="${OIDC_CLIENT_ID:-k8s-dashboard}"
OIDC_AUDIENCE="${OIDC_AUDIENCE:-$OIDC_CLIENT_ID}"
case "$AUTH_MODE" in
  oidc|none) ;;
  *) die "AUTH_MODE는 oidc 또는 none이어야 합니다: $AUTH_MODE" ;;
esac

ROOT="$(cd "$(dirname "$0")" && pwd)"
CHART="$ROOT/deploy/helm/observability-dashboard"
GIT_SHA="$(git -C "$ROOT" rev-parse --short HEAD)"
IMAGE_TAG="${IMAGE_TAG:-dev-$GIT_SHA}"
WEB_IMAGE="$REGISTRY/observability-dashboard-web"
API_IMAGE="$REGISTRY/observability-dashboard-api"

for cmd in docker helm kubectl; do
  command -v "$cmd" >/dev/null || die "$cmd 가 없습니다. WSL(Ubuntu)에서 실행하세요."
done
kubectl config get-contexts -o name | grep -qx "$CLUSTER" \
  || die "kubeconfig에 '$CLUSTER' 컨텍스트가 없습니다."

KC() { kubectl --context "$CLUSTER" "$@"; }

echo "== 배포 대상: context=$CLUSTER, scope=$SCOPE, release=$RELEASE_NAME/$RELEASE_NAMESPACE"
echo "== 이미지: $WEB_IMAGE:$IMAGE_TAG / $API_IMAGE:$IMAGE_TAG"

# 1) 이미지 빌드 + push
if [ "$SKIP_BUILD" != "1" ]; then
  echo "== [1/5] 이미지 빌드"
  docker build -f "$ROOT/Dockerfile.web" -t "$WEB_IMAGE:$IMAGE_TAG" "$ROOT"
  docker build -f "$ROOT/Dockerfile.api" -t "$API_IMAGE:$IMAGE_TAG" \
    --build-arg "VERSION=$IMAGE_TAG" --build-arg "COMMIT=$GIT_SHA" \
    --build-arg "BUILD_DATE=$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$ROOT"
  echo "== [2/5] 이미지 push → $REGISTRY"
  docker push "$WEB_IMAGE:$IMAGE_TAG"
  docker push "$API_IMAGE:$IMAGE_TAG"
else
  echo "== [1-2/5] SKIP_BUILD=1 — 빌드/push 생략"
fi

# 2) 대상 클러스터 네트워크 사실 수집 (NetworkPolicy 입력값)
echo "== [3/5] 클러스터 정보 수집"
API_SVC_IP="$(KC get svc kubernetes -n default -o jsonpath='{.spec.clusterIP}')"
API_EP_IPS="$(KC get endpoints kubernetes -n default -o jsonpath='{.subsets[0].addresses[*].ip}')"
API_EP_PORT="$(KC get endpoints kubernetes -n default -o jsonpath='{.subsets[0].ports[0].port}')"
IS_OPENSHIFT=0
KC api-versions | grep -q '^security.openshift.io/' && IS_OPENSHIFT=1
echo "   apiserver: svc=$API_SVC_IP endpoints=[$API_EP_IPS]:$API_EP_PORT openshift=$IS_OPENSHIFT"

# OIDC 기본 활성화(ADR 0011) — 브라우저 세션에는 HTTPS 공개 origin과 살아 있는
# issuer가 필요합니다. 확인에 실패하면 조용히 개방 배포로 낮추지 않고 중단합니다.
OIDC_ISSUER_VAL="${OIDC_ISSUER:-}"
OIDC_EGRESS_IP=""
OIDC_CA_PEM=""
if [ "$AUTH_MODE" = "oidc" ]; then
  if [ -z "$ROUTE_HOST" ] && [ "$IS_OPENSHIFT" = "1" ]; then
    ROUTE_HOST="$(KC -n "$RELEASE_NAMESPACE" get route "$RELEASE_NAME-ui" -o jsonpath='{.spec.host}' 2>/dev/null || true)"
  fi
  [ -n "$ROUTE_HOST" ] || die "AUTH_MODE=oidc에는 HTTPS 공개 origin이 필요합니다. ROUTE_HOST를 지정하거나 AUTH_MODE=none으로 명시적으로 끄세요."
  if [ -z "$OIDC_ISSUER_VAL" ]; then
    KC_HOST="$(KC get route -A --no-headers 2>/dev/null | awk 'tolower($2) ~ /keycloak/ {print $3; exit}')"
    [ -n "$KC_HOST" ] || die "keycloak Route를 찾지 못했습니다. OIDC_ISSUER를 직접 지정하거나 AUTH_MODE=none으로 끄세요."
    OIDC_ISSUER_VAL="https://$KC_HOST/realms/$OIDC_REALM"
  fi
  # discovery 문서의 issuer 문자열까지 정확히 일치해야 토큰 검증이 통과합니다.
  DISCOVERED_ISSUER="$(curl -skf --max-time 10 "$OIDC_ISSUER_VAL/.well-known/openid-configuration" \
    | python3 -c 'import json,sys; print(json.load(sys.stdin).get("issuer",""))' 2>/dev/null || true)"
  [ "$DISCOVERED_ISSUER" = "$OIDC_ISSUER_VAL" ] \
    || die "OIDC issuer 확인 실패: $OIDC_ISSUER_VAL (discovery 응답: ${DISCOVERED_ISSUER:-없음})"
  # issuer TLS 신뢰 확인 — 공용 CA로 검증되지 않으면(OKD edge Route 기본 인증서 등)
  # 클러스터 ingress CA로 재검증하고, 성공 시 그 CA를 API의 SSL_CERT_FILE 번들로 주입합니다.
  if ! curl -sf --max-time 10 "$OIDC_ISSUER_VAL/.well-known/openid-configuration" >/dev/null 2>&1; then
    OIDC_CA_PEM="$(KC -n openshift-config-managed get configmap default-ingress-cert -o jsonpath='{.data.ca-bundle\.crt}' 2>/dev/null || true)"
    [ -n "$OIDC_CA_PEM" ] || die "issuer TLS가 공용 CA로 검증되지 않고 클러스터 ingress CA도 찾지 못했습니다: $OIDC_ISSUER_VAL"
    CA_TMP="$(mktemp)"
    printf '%s\n' "$OIDC_CA_PEM" > "$CA_TMP"
    if ! curl -sf --cacert "$CA_TMP" --max-time 10 "$OIDC_ISSUER_VAL/.well-known/openid-configuration" >/dev/null 2>&1; then
      rm -f "$CA_TMP"
      die "클러스터 ingress CA로도 issuer TLS 검증에 실패했습니다: $OIDC_ISSUER_VAL"
    fi
    rm -f "$CA_TMP"
    echo "   oidc: issuer TLS를 클러스터 ingress CA로 신뢰합니다 (SSL_CERT_FILE 번들 주입)"
  fi
  # API → issuer egress(NetworkPolicy)용 IP — 라우터 VIP 등 issuer host의 A 레코드.
  ISSUER_HOST="$(printf '%s' "$OIDC_ISSUER_VAL" | sed -E 's#^https://([^/:]+).*#\1#')"
  OIDC_EGRESS_IP="$(getent hosts "$ISSUER_HOST" | awk '{print $1; exit}')"
  [ -n "$OIDC_EGRESS_IP" ] || die "issuer host($ISSUER_HOST)의 IP를 확인하지 못했습니다."
  echo "   oidc: issuer=$OIDC_ISSUER_VAL client=$OIDC_CLIENT_ID origin=https://$ROUTE_HOST egress=$OIDC_EGRESS_IP:443"
fi

# 데이터소스 자동 발견 — 클러스터 안의 GreptimeDB/Quickwit이 있으면 실데이터로
# 연결하고, 없으면 demo로 남습니다(fail-open). 대시보드는 조회 계층이므로
# 이 연결은 env와 NetworkPolicy egress로만 이루어집니다. (#30)
AUTO_DATASOURCES="${AUTO_DATASOURCES:-1}"
GREPTIME_URL_VAL="${GREPTIME_URL_OVERRIDE:-}"
GREPTIME_DB_VAL="${GREPTIME_DB_OVERRIDE:-public}"
QUICKWIT_URL_VAL="${QUICKWIT_URL_OVERRIDE:-}"
QUICKWIT_INDEX_VAL="${QUICKWIT_INDEX_OVERRIDE:-}"
GREPTIME_NS="" QUICKWIT_NS=""

# discover_svc <이름 패턴(소문자 regex)> <포트> → "namespace name" (첫 번째 일치)
discover_svc() {
  KC get svc -A --no-headers 2>/dev/null \
    | awk -v pat="$1" -v port="$2" 'tolower($2) ~ pat && index($6, port"/") > 0 {print $1, $2; exit}'
}

# svc_selector <ns> <svc> → 서비스의 spec.selector를 JSON 한 줄로. NetworkPolicy의
# podSelector로 그대로 씁니다 — 서비스가 고르는 Pod가 곧 egress 대상입니다.
svc_selector() {
  KC -n "$1" get svc "$2" -o json 2>/dev/null \
    | python3 -c 'import json,sys; print(json.dumps(json.load(sys.stdin)["spec"].get("selector") or {}))'
}

if [ "$AUTO_DATASOURCES" = "1" ]; then
  if [ -z "$GREPTIME_URL_VAL" ]; then
    read -r GREPTIME_NS GREPTIME_SVC <<< "$(discover_svc 'greptime' 4000)" || true
    if [ -n "${GREPTIME_SVC:-}" ]; then
      GREPTIME_URL_VAL="http://$GREPTIME_SVC.$GREPTIME_NS.svc.cluster.local:4000"
    fi
  fi
  if [ -z "$QUICKWIT_URL_VAL" ]; then
    # 검색 API를 받는 서비스를 우선합니다: searcher → control-plane 순.
    read -r QUICKWIT_NS QUICKWIT_SVC <<< "$(discover_svc 'quickwit.*searcher' 7280)" || true
    if [ -z "${QUICKWIT_SVC:-}" ]; then
      read -r QUICKWIT_NS QUICKWIT_SVC <<< "$(discover_svc 'quickwit.*control' 7280)" || true
    fi
    if [ -n "${QUICKWIT_SVC:-}" ]; then
      QUICKWIT_URL_VAL="http://$QUICKWIT_SVC.$QUICKWIT_NS.svc.cluster.local:7280"
      if [ -z "$QUICKWIT_INDEX_VAL" ]; then
        # 로그 인덱스 자동 감지 — otel-logs-v* 중 사전순 마지막(최신 스키마 버전).
        KC -n "$QUICKWIT_NS" port-forward "svc/$QUICKWIT_SVC" 17280:7280 >/dev/null 2>&1 &
        PF_PID=$!
        for _ in 1 2 3 4 5 6 7 8 9 10; do
          curl -sf http://localhost:17280/api/v1/version >/dev/null 2>&1 && break
          sleep 1
        done
        QUICKWIT_INDEX_VAL="$(curl -sf http://localhost:17280/api/v1/indexes 2>/dev/null \
          | python3 -c 'import json,sys; ids=sorted(i["index_config"]["index_id"] for i in json.load(sys.stdin)); c=[x for x in ids if x.startswith("otel-logs-v")]; print(c[-1] if c else "")' 2>/dev/null || true)"
        kill "$PF_PID" 2>/dev/null || true
        if [ -z "$QUICKWIT_INDEX_VAL" ]; then
          echo "   quickwit: otel-logs-v* 인덱스를 찾지 못해 로그 연결을 건너뜁니다"
          QUICKWIT_URL_VAL=""
        fi
      fi
    fi
  fi
fi
echo "   greptime: ${GREPTIME_URL_VAL:-(미발견 · demo)}  quickwit: ${QUICKWIT_URL_VAL:-(미발견 · demo)} index=${QUICKWIT_INDEX_VAL:-—}"

# 3) namespace + SCC + 보완 NetworkPolicy 선적용
echo "== [4/5] namespace/SCC/NetworkPolicy 준비"
KC get ns "$RELEASE_NAMESPACE" >/dev/null 2>&1 || KC create ns "$RELEASE_NAMESPACE"

if [ "$IS_OPENSHIFT" = "1" ]; then
  # chart는 ui/api/redis에 runAsUser(101/65532/999)를 고정하므로 restricted-v2
  # SCC에서 거부됩니다. nonroot-v2 사용 권한을 release ServiceAccount에 부여합니다.
  KC -n "$RELEASE_NAMESPACE" apply -f - <<EOF
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: $RELEASE_NAME-scc-nonroot-v2
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: system:openshift:scc:nonroot-v2
subjects:
  - {kind: ServiceAccount, name: $RELEASE_NAME-ui, namespace: $RELEASE_NAMESPACE}
  - {kind: ServiceAccount, name: $RELEASE_NAME-api, namespace: $RELEASE_NAMESPACE}
  - {kind: ServiceAccount, name: $RELEASE_NAME-redis, namespace: $RELEASE_NAMESPACE}
EOF
fi

# private registry pull 자격증명 — 로컬 docker 로그인 정보를 그대로 씁니다.
# chart가 imagePullSecrets를 지원하지 않으므로 helm 설치 후 SA에 패치합니다.
if [ -f "$HOME/.docker/config.json" ]; then
  KC -n "$RELEASE_NAMESPACE" create secret generic "$RELEASE_NAME-regcred" \
    --from-file=.dockerconfigjson="$HOME/.docker/config.json" \
    --type=kubernetes.io/dockerconfigjson \
    --dry-run=client -o yaml | KC -n "$RELEASE_NAMESPACE" apply -f -
fi

# Dashboard Builder(SQLite, ADR 0016)용 cursor key 시크릿 — 없을 때만 생성해
# 재배포·Delete→Deploy에도 같은 키를 유지합니다(진행 중 페이지네이션 cursor만 영향).
API_SECRET="$RELEASE_NAME-api"
if ! KC -n "$RELEASE_NAMESPACE" get secret "$API_SECRET" >/dev/null 2>&1; then
  KC -n "$RELEASE_NAMESPACE" create secret generic "$API_SECRET" \
    --from-literal=DASHBOARD_CURSOR_KEY="$(openssl rand -hex 32)"
fi

# 브라우저 세션 키(ADR 0011) — 32-byte base64url(무패딩). 없을 때만 만들어
# 재배포에도 기존 세션을 유지합니다. 키를 교체하면 전체 세션이 로그아웃됩니다.
if [ "$AUTH_MODE" = "oidc" ]; then
  if [ -z "$(KC -n "$RELEASE_NAMESPACE" get secret "$API_SECRET" -o jsonpath='{.data.AUTH_SESSION_KEY}')" ]; then
    KC -n "$RELEASE_NAMESPACE" patch secret "$API_SECRET" \
      -p "{\"stringData\":{\"AUTH_SESSION_KEY\":\"$(openssl rand 32 | basenc --base64url -w0 | tr -d '=')\"}}" >/dev/null
    echo "   secret: $API_SECRET 에 AUTH_SESSION_KEY 생성"
  fi
  if [ -n "${OIDC_CLIENT_SECRET:-}" ]; then
    KC -n "$RELEASE_NAMESPACE" patch secret "$API_SECRET" \
      -p "{\"stringData\":{\"OIDC_CLIENT_SECRET\":\"$OIDC_CLIENT_SECRET\"}}" >/dev/null
  fi
fi

# 사설 CA 번들 ConfigMap — 클러스터 ingress CA로 issuer TLS를 신뢰해야 할 때만 만듭니다.
OIDC_CA_CONFIGMAP=""
if [ -n "$OIDC_CA_PEM" ]; then
  OIDC_CA_CONFIGMAP="$RELEASE_NAME-oidc-ca"
  KC -n "$RELEASE_NAMESPACE" create configmap "$OIDC_CA_CONFIGMAP" \
    --from-literal=ca.crt="$OIDC_CA_PEM" --dry-run=client -o yaml | KC -n "$RELEASE_NAMESPACE" apply -f -
fi

# chart의 API egress 규칙은 TCP 443 고정이지만 OKD apiserver endpoint는 6443이고,
# OKD DNS는 openshift-dns의 5353 포트입니다. NetworkPolicy는 additive이므로
# 부족한 허용만 보완 정책 하나로 추가합니다.
EP_BLOCKS=""
for ip in $API_EP_IPS; do
  EP_BLOCKS="$EP_BLOCKS
        - ipBlock: {cidr: $ip/32}"
done
DNS_EGRESS=""
if [ "$IS_OPENSHIFT" = "1" ]; then
  DNS_EGRESS="
    - to:
        - namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: openshift-dns}}
          podSelector: {matchLabels: {dns.operator.openshift.io/daemonset-dns: default}}
      ports: [{protocol: UDP, port: 5353}, {protocol: TCP, port: 5353}]"
fi
KC -n "$RELEASE_NAMESPACE" apply -f - <<EOF
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: $RELEASE_NAME-cluster-compat
spec:
  podSelector:
    matchLabels:
      app.kubernetes.io/instance: $RELEASE_NAME
  policyTypes: [Egress]
  egress:
    - to:$EP_BLOCKS
      ports: [{protocol: TCP, port: $API_EP_PORT}]$DNS_EGRESS
EOF

# 4) helm 설치/업그레이드 — 동적 값은 overlay values로 주입
OVERLAY="$(mktemp)"
trap 'rm -f "$OVERLAY"' EXIT
# OTel 표준 로그 인덱스(otel-logs-v*)의 필드 경로 매핑입니다. 나머지 키는 어댑터
# 기본값을 씁니다. workload_kind는 OTel 표준 속성에 없어 매핑하지 않습니다.
OTEL_QUICKWIT_FIELDS="timestamp=timestamp_nanos,level=severity_text,message=body.message,namespace=resource_attributes.k8s.namespace.name,pod_name=resource_attributes.k8s.pod.name,pod_uid=resource_attributes.k8s.pod.uid,container=resource_attributes.k8s.container.name,workload_name=resource_attributes.k8s.deployment.name,node=resource_attributes.k8s.node.name,trace_id=trace_id,span_id=span_id"

{
  cat <<EOF
fullnameOverride: $RELEASE_NAME
ui:
  image: {repository: $WEB_IMAGE, tag: $IMAGE_TAG}
api:
  image: {repository: $API_IMAGE, tag: $IMAGE_TAG}
  config:
    CLUSTER_ID: "$CLUSTER"
    CLUSTER_NAME: "$CLUSTER"
    SCOPE_NAMESPACES: "$SCOPE"
EOF
  if [ "$AUTH_MODE" = "oidc" ]; then
    cat <<EOF
    AUTH_MODE: "oidc"
    OIDC_ISSUER: "$OIDC_ISSUER_VAL"
    OIDC_AUDIENCE: "$OIDC_AUDIENCE"
EOF
    if [ -n "$OIDC_CA_CONFIGMAP" ]; then
      cat <<EOF
    SSL_CERT_FILE: "/etc/dashboard-ca/ca.crt"
EOF
    fi
  fi
  if [ -n "$GREPTIME_URL_VAL" ]; then
    cat <<EOF
    GREPTIME_URL: "$GREPTIME_URL_VAL"
    GREPTIME_DB: "$GREPTIME_DB_VAL"
EOF
  fi
  if [ -n "$QUICKWIT_URL_VAL" ]; then
    cat <<EOF
    QUICKWIT_URL: "$QUICKWIT_URL_VAL"
    QUICKWIT_INDEX: "$QUICKWIT_INDEX_VAL"
    QUICKWIT_FIELDS: "$OTEL_QUICKWIT_FIELDS"
EOF
  fi
  cat <<EOF
  existingSecret:
    name: $API_SECRET
EOF
  if [ -n "$OIDC_CA_CONFIGMAP" ]; then
    cat <<EOF
  caBundle: {configMapName: $OIDC_CA_CONFIGMAP, key: ca.crt}
EOF
  fi
  cat <<EOF
manageWorkloads:
  # Deployment/Secret 관리 탭(ADR 0014). 기본 OIDC 모드에서는 platform.admin 역할이
  # 부여된 계정에만 열립니다. AUTH_MODE=none이면 게이팅 없이 열리니 유의하세요.
  enabled: true
dashboardBuilder:
  # Custom Dashboard Builder(ADR 0016). SQLite 파일을 PVC에 두어 재배포에도 draft가 보존됩니다.
  # 단일 writer라 API는 replicas=1 + Recreate로 강제됩니다. cursor key는 위에서 만든 시크릿에서 옵니다.
  enabled: true
  sqlite:
    enabled: true
redis:
  # OKD cri-o는 short name을 거부하므로 fully-qualified 경로를 씁니다.
  image: {repository: docker.io/library/redis}
EOF
  if [ "$AUTH_MODE" = "oidc" ]; then
    # 브라우저 세션(ADR 0011). TLS 게시는 chart Ingress가 아니라 OKD Route가 맡습니다.
    cat <<EOF
authSession:
  enabled: true
  externalIngress: true
  publicOrigin: "https://$ROUTE_HOST"
  redirectURI: "https://$ROUTE_HOST/api/v1/auth/callback"
  clientID: "$OIDC_CLIENT_ID"
EOF
  fi
  cat <<EOF
networkPolicy:
  ingress:
    cidrs: ["10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16"]
  kubernetesApiCidrs:
    - $API_SVC_IP/32
EOF
  for ip in $API_EP_IPS; do echo "    - $ip/32"; done
  if [ "$AUTH_MODE" = "oidc" ]; then
    cat <<EOF
  external:
    - {cidr: $OIDC_EGRESS_IP/32, port: 443, protocol: TCP, purpose: oidc}
EOF
  fi
  # 발견된 데이터소스로의 egress만 엽니다. namespace를 아는 자동 발견 경로에서만
  # 규칙을 만들 수 있습니다(URL 수동 지정 시에는 NetworkPolicy를 직접 관리하세요).
  if [ -n "$GREPTIME_NS$QUICKWIT_NS" ]; then
    echo "  internal:"
    if [ -n "$GREPTIME_NS" ]; then
      cat <<EOF
    - namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: $GREPTIME_NS}}
      podSelector: {matchLabels: $(svc_selector "$GREPTIME_NS" "$GREPTIME_SVC")}
      port: 4000
EOF
    fi
    if [ -n "$QUICKWIT_NS" ]; then
      cat <<EOF
    - namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: $QUICKWIT_NS}}
      podSelector: {matchLabels: $(svc_selector "$QUICKWIT_NS" "$QUICKWIT_SVC")}
      port: 7280
EOF
    fi
  fi
  if [ "$IS_OPENSHIFT" = "1" ]; then
    cat <<'EOF'
  dns:
    namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: openshift-dns}}
    podSelector: {matchLabels: {dns.operator.openshift.io/daemonset-dns: default}}
EOF
  fi
} > "$OVERLAY"

echo "== [5/5] helm upgrade --install"
helm upgrade --install "$RELEASE_NAME" "$CHART" \
  --kube-context "$CLUSTER" \
  --namespace "$RELEASE_NAMESPACE" \
  -f "$CHART/values-dev.yaml" \
  -f "$OVERLAY" \
  --timeout 5m

# imagePullSecrets는 Pod 생성 시점에 복사되므로, SA 패치 후 재기동해야 반영됩니다.
if KC -n "$RELEASE_NAMESPACE" get secret "$RELEASE_NAME-regcred" >/dev/null 2>&1; then
  for sa in "$RELEASE_NAME-ui" "$RELEASE_NAME-api" "$RELEASE_NAME-redis"; do
    KC -n "$RELEASE_NAMESPACE" patch sa "$sa" \
      -p "{\"imagePullSecrets\":[{\"name\":\"$RELEASE_NAME-regcred\"}]}"
  done
  KC -n "$RELEASE_NAMESPACE" rollout restart \
    "deploy/$RELEASE_NAME-ui" "deploy/$RELEASE_NAME-api" "statefulset/$RELEASE_NAME-redis"
fi

for target in "deploy/$RELEASE_NAME-ui" "deploy/$RELEASE_NAME-api" "statefulset/$RELEASE_NAME-redis"; do
  KC -n "$RELEASE_NAMESPACE" rollout status "$target" --timeout 5m
done

# OpenShift Route — ROUTE_HOST가 지정된 경우에만 상시 URL을 만듭니다.
if [ -n "$ROUTE_HOST" ] && [ "$IS_OPENSHIFT" = "1" ]; then
  KC -n "$RELEASE_NAMESPACE" apply -f - <<EOF
apiVersion: route.openshift.io/v1
kind: Route
metadata:
  name: $RELEASE_NAME-ui
  labels:
    app.kubernetes.io/instance: $RELEASE_NAME
spec:
  host: $ROUTE_HOST
  to: {kind: Service, name: $RELEASE_NAME-ui}
  port: {targetPort: http}
  tls: {termination: edge, insecureEdgeTerminationPolicy: Redirect}
EOF
fi

echo
echo "== 배포 완료. Pod 상태:"
KC -n "$RELEASE_NAMESPACE" get pods
echo
echo "접속 방법:"
if [ -n "$ROUTE_HOST" ] && [ "$IS_OPENSHIFT" = "1" ]; then
  echo "  https://$ROUTE_HOST"
else
  echo "  kubectl --context $CLUSTER -n $RELEASE_NAMESPACE port-forward svc/$RELEASE_NAME-ui 18080:8080"
  echo "  → http://localhost:18080"
fi
if [ "$AUTH_MODE" = "oidc" ]; then
  echo "  로그인: $OIDC_ISSUER_VAL 계정 중 '$OIDC_CLIENT_ID' client role(platform.admin 등)이"
  echo "  부여된 계정만 접근할 수 있습니다. 세션은 HTTPS origin에서만 동작하므로"
  echo "  port-forward(http://localhost)로는 로그인할 수 없습니다."
fi
