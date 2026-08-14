#!/usr/bin/env sh
set -eu

ROOT=$(CDPATH= cd -- "$(dirname "$0")/../.." && pwd)
CHART="$ROOT/deploy/helm/observability-dashboard"
TMP_ROOT="$ROOT/.tmp"
mkdir -p "$TMP_ROOT"
TMP=$(mktemp -d "$TMP_ROOT/deploy.XXXXXX")
trap 'rm -rf "$TMP"' EXIT HUP INT TERM

HELM_IMAGE='alpine/helm:3.17.3@sha256:d899e6316789fec04ee95300a18e454b7942539cbb3d89bde3e0655d6ca2e895'
KUBECONFORM_IMAGE='ghcr.io/yannh/kubeconform:v0.6.7@sha256:0925177fb05b44ce18574076141b5c3d83235e1904d3f952182ac99ddc45762c'

docker run --rm -v "$ROOT:/work:ro" "$HELM_IMAGE" lint /work/deploy/helm/observability-dashboard
for env in dev stage prod; do
  docker run --rm -v "$ROOT:/work:ro" "$HELM_IMAGE" template release-name /work/deploy/helm/observability-dashboard \
    --namespace observability --values "/work/deploy/helm/observability-dashboard/values-$env.yaml" > "$TMP/$env.yaml"
  docker run --rm -v "$ROOT:/work:ro" "$HELM_IMAGE" template release-name /work/deploy/helm/observability-dashboard \
    --namespace observability --values "/work/deploy/helm/observability-dashboard/values-$env.yaml" > "$TMP/$env.second.yaml"
  cmp "$TMP/$env.yaml" "$TMP/$env.second.yaml"
  docker run --rm -i "$KUBECONFORM_IMAGE" -strict -summary -kubernetes-version 1.31.0 < "$TMP/$env.yaml"
  python3 "$ROOT/scripts/check-deploy.py" "$TMP/$env.yaml" --environment "$env" --self-test
  if [ "$env" = dev ]; then
    ! grep -q 'prometheus.io/scrape' "$TMP/$env.yaml"
    ! grep -q 'grafana-dashboard' "$TMP/$env.yaml"
  else
    grep -q 'prometheus.io/scrape: "true"' "$TMP/$env.yaml"
    grep -q 'grafana-dashboard' "$TMP/$env.yaml"
    grep -q 'app.kubernetes.io/name: prometheus' "$TMP/$env.yaml"
  fi
done

expect_render_failure() {
  if docker run --rm -v "$ROOT:/work:ro" "$HELM_IMAGE" template release-name /work/deploy/helm/observability-dashboard \
    --namespace observability --values /work/deploy/helm/observability-dashboard/values-dev.yaml "$@" >/dev/null 2>&1; then
    echo "negative values mutation unexpectedly rendered: $*" >&2
    exit 1
  fi
}
expect_render_failure --set-json 'networkPolicy.ingress.cidrs=[]'
expect_render_failure --set 'networkPolicy.monitoring.enabled=true'
expect_render_failure --set-json 'networkPolicy.dns.namespaceSelector=null'

python3 - "$TMP" <<'PY'
import hashlib
import re
import sys
from collections import Counter
from pathlib import Path

root = Path(sys.argv[1])
counts = {}
for env in ("dev", "stage", "prod"):
    data = (root / f"{env}.yaml").read_bytes()
    kinds = Counter(re.findall(rb"^kind: (.+)$", data, re.M))
    counts[env] = kinds
    print(f"{env}: objects={sum(kinds.values())} sha256={hashlib.sha256(data).hexdigest()}")
required = {b"Deployment", b"Service", b"ConfigMap", b"ClusterRole", b"ClusterRoleBinding", b"NetworkPolicy"}
for env, kinds in counts.items():
    missing = required - kinds.keys()
    if missing:
        raise SystemExit(f"{env}: missing common kinds: {sorted(missing)}")
PY
