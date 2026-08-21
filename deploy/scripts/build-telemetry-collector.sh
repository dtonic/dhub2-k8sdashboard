#!/usr/bin/env sh
set -eu

ROOT=$(CDPATH= cd -- "$(dirname "$0")/../.." && pwd)
BASE=$(realpath "${TMPDIR:-/tmp}")
TMP=$(mktemp -d "$BASE/issue23-ocb.XXXXXX")
IMAGE=${TELEMETRY_COLLECTOR_IMAGE:?set TELEMETRY_COLLECTOR_IMAGE to a unique unused local tag}
IMAGE_ID=
SUCCESS=0
cleanup() {
  if [ "$SUCCESS" -ne 1 ] && [ -n "$IMAGE_ID" ] && [ "$(docker image inspect -f '{{.Id}}' "$IMAGE" 2>/dev/null || true)" = "$IMAGE_ID" ]; then
    docker image rm "$IMAGE" >/dev/null
  fi
  resolved=$(realpath "$TMP" 2>/dev/null || true)
  case "$resolved" in "$BASE"/issue23-ocb.*) [ ! -d "$resolved" ] || rm -rf -- "$resolved" ;; *) return 1 ;; esac
}
trap cleanup EXIT HUP INT TERM

if docker image inspect "$IMAGE" >/dev/null 2>&1; then
  echo "refusing to overwrite existing collector image tag: $IMAGE" >&2
  exit 1
fi

docker build --quiet --target evidence --output "type=local,dest=$TMP/evidence" \
  -f "$ROOT/deploy/telemetry/Dockerfile.collector" "$ROOT/deploy/telemetry"
check_hash() {
  actual=$(sha256sum "$TMP/evidence/$1" | cut -d' ' -f1)
  [ "$actual" = "$2" ] || { echo "$1 reproducibility hash mismatch: $actual" >&2; return 1; }
}
check_hash go.mod bff6e429b67b94bc95659387ac4240fa19c3ca3f49e7a8afcd2a1dbc35ccd442
check_hash go.sum 42d03618eaf737b778612108b0352a506ea3625830189dd5a77f8f44c7dcf503
check_hash dashboard-otelcol c544e7cf18f0f44c82917877dd664dda943f52a36a1dda1d948f94a5244f5030
if check_hash dashboard-otelcol 0000000000000000000000000000000000000000000000000000000000000000 2>/dev/null; then
  echo "collector hash negative mutation unexpectedly passed" >&2
  exit 1
fi

docker run --rm --mount "type=bind,source=$TMP/evidence/dashboard-otelcol,target=/bin/collector,readonly" \
  golang:1.26.6-alpine@sha256:af8d6740070b8906d12eae1c3e3ea0957fb63f492051ea05e354c38ef9fe88df \
  go version -m /bin/collector > "$TMP/go-version.txt"
grep -q '^/bin/collector: go1.26.6$' "$TMP/go-version.txt"
grep -q 'build.*-trimpath=true' "$TMP/go-version.txt"
grep -q 'build.*CGO_ENABLED=0' "$TMP/go-version.txt"

docker build --quiet -f "$ROOT/deploy/telemetry/Dockerfile.collector" -t "$IMAGE" "$ROOT/deploy/telemetry"
IMAGE_ID=$(docker image inspect -f '{{.Id}}' "$IMAGE")
docker run --rm "$IMAGE" components > "$TMP/components.yaml"
PYTHONDONTWRITEBYTECODE=1 python3 -B - "$TMP/components.yaml" <<'PY'
import sys
from pathlib import Path
expected = {
    "receivers": {"file_log", "host_metrics", "k8s_cluster", "otlp", "prometheus"},
    "processors": {"batch", "filter", "k8s_attributes", "memory_limiter", "resource", "transform"},
    "exporters": {"nop", "otlp_grpc", "otlp_http"},
    "connectors": set(), "extensions": {"health_check"},
}
found = {key: set() for key in expected}
section = None
for line in Path(sys.argv[1]).read_text().splitlines():
    if line.endswith(":") and line[:-1] in expected:
        section = line[:-1]
    elif section and line.startswith("    - name: "):
        found[section].add(line.split(": ", 1)[1])
if found != expected:
    raise SystemExit(f"collector component drift: {found}")
PY
SUCCESS=1
echo "telemetry collector build passed: image=$IMAGE binary_sha256=c544e7cf18f0f44c82917877dd664dda943f52a36a1dda1d948f94a5244f5030"
