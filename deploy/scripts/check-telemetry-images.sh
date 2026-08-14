#!/usr/bin/env sh
set -eu

ROOT=$(CDPATH= cd -- "$(dirname "$0")/../.." && pwd)
BASE=$(realpath "${TMPDIR:-/tmp}")
TMP=$(mktemp -d "$BASE/issue23-images.XXXXXX")
TOKEN=$(printf '%s' "${TMP##*.}" | tr '[:upper:]' '[:lower:]')
MOCK_CONTAINER="issue23-mock-scan-$TOKEN"
MOCK_ID=
COLLECTOR_IMAGE=${TELEMETRY_COLLECTOR_IMAGE:-issue23-otelcol-$TOKEN:ci}
MOCK_IMAGE=${TELEMETRY_MOCK_IMAGE:-issue23-mock-image-$TOKEN:ci}
COLLECTOR_ID=
MOCK_IMAGE_ID=
TRIVY='aquasec/trivy:0.74.0@sha256:62b1e65e8869bc4b4c6aa4fa2b21595256c7c2f6018a9d9ad61caf87187c1969'
TRIVY_USER="$(id -u):$(id -g)"
CACHE=${TELEMETRY_TRIVY_CACHE:-$TMP/trivy-cache}
cleanup() {
  if [ -n "$MOCK_ID" ] && [ "$(docker inspect -f '{{.Id}}' "$MOCK_CONTAINER" 2>/dev/null || true)" = "$MOCK_ID" ]; then
    docker rm -f "$MOCK_CONTAINER" >/dev/null
  fi
  if [ -n "$COLLECTOR_ID" ] && [ "$(docker image inspect -f '{{.Id}}' "$COLLECTOR_IMAGE" 2>/dev/null || true)" = "$COLLECTOR_ID" ]; then docker image rm "$COLLECTOR_IMAGE" >/dev/null; fi
  if [ -n "$MOCK_IMAGE_ID" ] && [ "$(docker image inspect -f '{{.Id}}' "$MOCK_IMAGE" 2>/dev/null || true)" = "$MOCK_IMAGE_ID" ]; then docker image rm "$MOCK_IMAGE" >/dev/null; fi
  resolved=$(realpath "$TMP" 2>/dev/null || true)
  case "$resolved" in
    "$BASE"/issue23-images.*)
      if [ -d "$resolved" ]; then chmod -R u+rwX "$resolved"; rm -rf -- "$resolved"; fi
      ;;
    *) return 1 ;;
  esac
}
trap cleanup EXIT HUP INT TERM
if [ -n "${TELEMETRY_TRIVY_CACHE:-}" ]; then
  CACHE_ROOT=$(realpath "${TELEMETRY_TRIVY_CACHE_ROOT:?set TELEMETRY_TRIVY_CACHE_ROOT with shared cache}")
  CACHE=$(realpath "$CACHE")
  case "$CACHE" in "$CACHE_ROOT"/*) : ;; *) echo "shared Trivy cache is outside its declared temp root" >&2; exit 1 ;; esac
fi
mkdir -p "$CACHE"

if docker image inspect "$COLLECTOR_IMAGE" >/dev/null 2>&1 || docker image inspect "$MOCK_IMAGE" >/dev/null 2>&1; then
  echo "refusing to overwrite existing telemetry image tag" >&2
  exit 1
fi
TELEMETRY_COLLECTOR_IMAGE="$COLLECTOR_IMAGE" sh "$ROOT/deploy/scripts/build-telemetry-collector.sh"
COLLECTOR_ID=$(docker image inspect -f '{{.Id}}' "$COLLECTOR_IMAGE")
docker build --quiet -f "$ROOT/deploy/telemetry/Dockerfile.mock" -t "$MOCK_IMAGE" "$ROOT"
MOCK_IMAGE_ID=$(docker image inspect -f '{{.Id}}' "$MOCK_IMAGE")

docker save --output "$TMP/collector.tar" "$COLLECTOR_IMAGE"
docker run --rm --user "$TRIVY_USER" -v "$TMP:/scan:ro" -v "$CACHE:/tmp/trivy-cache" "$TRIVY" image \
  --cache-dir /tmp/trivy-cache --input /scan/collector.tar --scanners vuln \
  --severity HIGH,CRITICAL --exit-code 1 --no-progress

if docker inspect "$MOCK_CONTAINER" >/dev/null 2>&1; then
  echo "refusing to reuse existing mock scan container: $MOCK_CONTAINER" >&2
  exit 1
fi
MOCK_ID=$(docker create --name "$MOCK_CONTAINER" "$MOCK_IMAGE")
docker export --output "$TMP/mock-rootfs.tar" "$MOCK_CONTAINER"
mkdir -p "$TMP/mock-rootfs"
tar -xf "$TMP/mock-rootfs.tar" -C "$TMP/mock-rootfs"
test -f "$TMP/mock-rootfs/lib/apk/db/installed"
grep -q '^P:python3$' "$TMP/mock-rootfs/lib/apk/db/installed"
grep -q '^V:3.14.7-r0$' "$TMP/mock-rootfs/lib/apk/db/installed"
test ! -e "$TMP/mock-rootfs/usr/lib/python3.14/site-packages/pip"
test ! -e "$TMP/mock-rootfs/usr/lib/python3.14/site-packages/msgpack"
test ! -e "$TMP/mock-rootfs/usr/lib/python3.14/site-packages/setuptools-70.3.0.dist-info"
docker run --rm --user "$TRIVY_USER" -v "$TMP/mock-rootfs:/rootfs:ro" -v "$CACHE:/tmp/trivy-cache" "$TRIVY" rootfs \
  --cache-dir /tmp/trivy-cache --scanners vuln --severity HIGH,CRITICAL --exit-code 1 --no-progress /rootfs

negative="$TMP/mock-rootfs/usr/lib/python3.14/site-packages/setuptools-70.3.0.dist-info"
mkdir -p "$negative"
printf '%s\n' 'Metadata-Version: 2.1' 'Name: setuptools' 'Version: 70.3.0' > "$negative/METADATA"
if docker run --rm --user "$TRIVY_USER" -v "$TMP/mock-rootfs:/rootfs:ro" -v "$CACHE:/tmp/trivy-cache" "$TRIVY" rootfs \
  --cache-dir /tmp/trivy-cache --scanners vuln --severity HIGH,CRITICAL \
  --exit-code 1 --no-progress /rootfs > "$TMP/negative.log" 2>&1; then
  echo "vulnerable Python package mutation was not rejected" >&2
  exit 1
fi
grep -q 'CVE-2025-47273' "$TMP/negative.log"
grep -q 'setuptools' "$TMP/negative.log"
rm -- "$negative/METADATA"
rmdir -- "$negative"
echo "telemetry images passed: collector/mock filesystem HIGH=0 CRITICAL=0; vulnerable fixture rejected"
