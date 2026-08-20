#!/usr/bin/env bash
# delete_deploy_dashboard.sh — deploy.sh가 설치한 대시보드 제거 (Issue #26)
#
# 사용법:
#   ./scripts/delete_deploy_dashboard.sh <cluster-context>
#
# 예시:
#   ./scripts/delete_deploy_dashboard.sh lnode                    # release + 부속 리소스 삭제
#   PURGE_NAMESPACE=1 ./scripts/delete_deploy_dashboard.sh lnode  # namespace까지 통째로 삭제
#
# 환경변수 (선택):
#   RELEASE_NAME       helm release 이름 (기본: dashboard) — deploy.sh와 같은 값 사용
#   RELEASE_NAMESPACE  설치 namespace    (기본: observability)
#   PURGE_NAMESPACE    1이면 namespace 자체를 삭제. 기본은 남겨둡니다 —
#                      다른 워크로드가 함께 들어있을 수 있기 때문입니다.
#
# 삭제 대상:
#   1. helm release (UI/API/Redis, Service, ConfigMap, RBAC, NetworkPolicy 등 chart 소유 리소스)
#   2. deploy.sh가 chart 밖에서 만든 부속 리소스:
#      - RoleBinding  <release>-scc-nonroot-v2   (OKD SCC)
#      - NetworkPolicy <release>-cluster-compat  (apiserver/DNS egress 보완)
#      - Secret       <release>-regcred          (private registry pull)
#      - Route        <release>-ui               (OpenShift 상시 URL, 있을 때만)
#
# registry에 push된 이미지는 삭제하지 않습니다 (필요 시 Harbor에서 수동 삭제).
# WSL(Ubuntu) bash 전용입니다. helm / kubectl이 필요합니다.
set -euo pipefail

die() { echo "오류: $*" >&2; exit 1; }

[ $# -ge 1 ] || die "사용법: ./delete_deploy_dashboard.sh <cluster-context>"
CLUSTER="$1"

RELEASE_NAME="${RELEASE_NAME:-dashboard}"
RELEASE_NAMESPACE="${RELEASE_NAMESPACE:-observability}"
PURGE_NAMESPACE="${PURGE_NAMESPACE:-0}"

for cmd in helm kubectl; do
  command -v "$cmd" >/dev/null || die "$cmd 가 없습니다. WSL(Ubuntu)에서 실행하세요."
done
kubectl config get-contexts -o name | grep -qx "$CLUSTER" \
  || die "kubeconfig에 '$CLUSTER' 컨텍스트가 없습니다."

KC() { kubectl --context "$CLUSTER" "$@"; }

echo "== 제거 대상: context=$CLUSTER, release=$RELEASE_NAME/$RELEASE_NAMESPACE"

if ! KC get ns "$RELEASE_NAMESPACE" >/dev/null 2>&1; then
  echo "namespace '$RELEASE_NAMESPACE' 가 없습니다. 제거할 것이 없습니다."
  exit 0
fi

# 1) helm release
if helm status "$RELEASE_NAME" --kube-context "$CLUSTER" -n "$RELEASE_NAMESPACE" >/dev/null 2>&1; then
  echo "== [1/3] helm uninstall $RELEASE_NAME"
  helm uninstall "$RELEASE_NAME" --kube-context "$CLUSTER" -n "$RELEASE_NAMESPACE" --wait --timeout 5m
else
  echo "== [1/3] helm release '$RELEASE_NAME' 없음 — 건너뜀"
fi

# 2) deploy.sh가 chart 밖에서 만든 부속 리소스
echo "== [2/3] 부속 리소스 삭제"
KC -n "$RELEASE_NAMESPACE" delete rolebinding "$RELEASE_NAME-scc-nonroot-v2" --ignore-not-found
KC -n "$RELEASE_NAMESPACE" delete networkpolicy "$RELEASE_NAME-cluster-compat" --ignore-not-found
KC -n "$RELEASE_NAMESPACE" delete secret "$RELEASE_NAME-regcred" --ignore-not-found
if KC api-versions | grep -q '^route.openshift.io/'; then
  KC -n "$RELEASE_NAMESPACE" delete route "$RELEASE_NAME-ui" --ignore-not-found
fi

# 3) namespace (opt-in)
if [ "$PURGE_NAMESPACE" = "1" ]; then
  echo "== [3/3] namespace '$RELEASE_NAMESPACE' 삭제"
  KC delete ns "$RELEASE_NAMESPACE"
else
  REMAIN="$(KC -n "$RELEASE_NAMESPACE" get pods --no-headers 2>/dev/null | wc -l)"
  echo "== [3/3] namespace는 남겨둡니다 (남은 Pod: $REMAIN). 통째로 지우려면:"
  echo "   PURGE_NAMESPACE=1 ./delete_deploy_dashboard.sh $CLUSTER"
fi

echo
echo "== 제거 완료. registry의 이미지(lnode/observability-dashboard-*)는 남아 있습니다."
