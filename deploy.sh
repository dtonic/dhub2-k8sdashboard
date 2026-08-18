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
#   ROUTE_HOST         OpenShift/OKD에서 UI를 노출할 Route 호스트명
#                      (예: k8sdashboard.apps.okd.dtonic.io). 비우면 Route를 만들지
#                      않습니다. 현재 dev 모드는 AUTH_MODE=none이므로 이 URL은
#                      사내망 누구나 접근 가능합니다 — 조회 전용이지만 유의하세요.
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
redis:
  # OKD cri-o는 short name을 거부하므로 fully-qualified 경로를 씁니다.
  image: {repository: docker.io/library/redis}
networkPolicy:
  ingress:
    cidrs: ["10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16"]
  kubernetesApiCidrs:
    - $API_SVC_IP/32
EOF
  for ip in $API_EP_IPS; do echo "    - $ip/32"; done
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
