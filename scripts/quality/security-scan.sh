#!/usr/bin/env sh
set -eu

ROOT=$(CDPATH= cd -- "$(dirname "$0")/../.." && pwd)
TMP_ROOT=${RUNNER_TEMP:-${TMPDIR:-/tmp}}
TMP=$(mktemp -d "$TMP_ROOT/dashboard-security.XXXXXX")
cleanup() {
  resolved=$(realpath "$TMP" 2>/dev/null || true)
  base=$(realpath "$TMP_ROOT")
  case "$resolved" in
    "$base"/dashboard-security.*) [ ! -d "$resolved" ] || rm -rf -- "$resolved" ;;
    *) echo "refusing to clean unexpected security temp path: $resolved" >&2 ;;
  esac
  docker image rm observability-dashboard-web-builder:ci >/dev/null 2>&1 || true
}
trap cleanup EXIT HUP INT TERM
SOURCE="$TMP/source"
TRIVY_CACHE="$TMP/trivy-cache"
mkdir -p "$SOURCE" "$TRIVY_CACHE"

GITLEAKS_IMAGE='ghcr.io/gitleaks/gitleaks:v8.30.1@sha256:c00b6bd0aeb3071cbcb79009cb16a60dd9e0a7c60e2be9ab65d25e6bc8abbb7f'
TRIVY_IMAGE='aquasec/trivy:0.74.0@sha256:62b1e65e8869bc4b4c6aa4fa2b21595256c7c2f6018a9d9ad61caf87187c1969'
HELM_IMAGE='alpine/helm:3.17.3@sha256:d899e6316789fec04ee95300a18e454b7942539cbb3d89bde3e0655d6ca2e895'
POSTGRES_IMAGE='cgr.dev/chainguard/postgres@sha256:844baac51caa0212727f9a53f25beec94cedb6778c06c75e3f7bb092079142f3'
TRIVY_USER="$(id -u):$(id -g)"

is_secret_env_path() {
  case "${1##*/}" in
    .env) return 0 ;;
    .env.*) [ "${1##*/}" != ".env.example" ] ;;
    *) return 1 ;;
  esac
}
if is_secret_env_path ".env.example" || ! is_secret_env_path ".env" || ! is_secret_env_path "nested/.env.local"; then
  echo "environment filename policy self-test failed" >&2
  exit 1
fi

# Scan every committed file. Untracked candidates remain constrained to product
# pathspecs so local-only paths outside the repository product are never listed.
tracked_env=$(git -C "$ROOT" ls-files --cached | while IFS= read -r path; do
  if is_secret_env_path "$path"; then
    printf '%s\n' "$path"
    break
  fi
done)
if [ -n "$tracked_env" ]; then
  echo "refusing to copy a tracked .env file into the scan tree" >&2
  exit 1
fi
{
  git -C "$ROOT" ls-files --cached
  git -C "$ROOT" ls-files --others --exclude-standard -- .github apps deploy design-system docs packages quality scripts .dockerignore .gitignore Makefile README.md go.work package.json package-lock.json
} | sort -u | while IFS= read -r path; do
  if is_secret_env_path "$path"; then
    continue
  fi
  case "$path" in
    apps/api/cover|apps/api/coverage|apps/api/quality-cover|*.out) continue ;;
  esac
  if [ -L "$ROOT/$path" ]; then
    echo "refusing to scan symlink outside the copied product tree: $path" >&2
    exit 1
  fi
  mkdir -p "$SOURCE/$(dirname "$path")"
  cp "$ROOT/$path" "$SOURCE/$path"
done
if git -C "$ROOT" ls-files --error-unmatch -- .env.example >/dev/null 2>&1 && [ ! -f "$SOURCE/.env.example" ]; then
  echo "tracked .env.example was not copied into the scan tree" >&2
  exit 1
fi

docker run --rm -v "$SOURCE:/repo:ro" "$GITLEAKS_IMAGE" dir /repo --no-banner --redact --verbose
printf '%s\n' 'api_key = sk-live-7f3ac91b22d4a5c6e7f890abcdef' > "$SOURCE/negative-secret.txt" # gitleaks:allow
if docker run --rm -v "$SOURCE:/repo:ro" "$GITLEAKS_IMAGE" dir /repo --no-banner --redact --verbose >"$TMP/gitleaks-negative.log" 2>&1; then
  echo "fake secret mutation was not rejected" >&2
  exit 1
fi
if ! grep -q 'negative-secret.txt' "$TMP/gitleaks-negative.log" || ! grep -q 'generic-api-key' "$TMP/gitleaks-negative.log"; then
  sed -n '1,120p' "$TMP/gitleaks-negative.log" >&2
  echo "fake secret mutation failed without the expected gitleaks finding" >&2
  exit 1
fi
rm -f "$SOURCE/negative-secret.txt"
echo "negative mutation passed: fake secret was rejected"

docker run --rm --user "$TRIVY_USER" -v "$SOURCE:/repo:ro" -v "$TRIVY_CACHE:/tmp/trivy-cache" "$TRIVY_IMAGE" fs --cache-dir /tmp/trivy-cache --scanners vuln --severity HIGH,CRITICAL --exit-code 1 --no-progress /repo
docker run --rm --user "$TRIVY_USER" -v "$SOURCE:/repo:ro" -v "$TRIVY_CACHE:/tmp/trivy-cache" "$TRIVY_IMAGE" fs --cache-dir /tmp/trivy-cache --scanners misconfig --severity HIGH,CRITICAL --exit-code 1 --no-progress /repo
# The agent chart is intentionally fail-closed with unusable defaults, so scan
# its validated example render explicitly instead of relying on chart discovery.
docker run --rm -v "$SOURCE:/repo:ro" "$HELM_IMAGE" template agent /repo/deploy/helm/cluster-state-agent \
  --values /repo/deploy/helm/cluster-state-agent/values-example.yaml > "$TMP/cluster-state-agent.yaml"
docker run --rm --user "$TRIVY_USER" -v "$TMP:/scan:ro" -v "$TRIVY_CACHE:/tmp/trivy-cache" "$TRIVY_IMAGE" \
  config --cache-dir /tmp/trivy-cache --severity HIGH,CRITICAL --exit-code 1 /scan/cluster-state-agent.yaml
docker pull "$POSTGRES_IMAGE" >/dev/null
docker image inspect --format '{{range .RepoDigests}}{{println .}}{{end}}' "$POSTGRES_IMAGE" | grep -Fx "$POSTGRES_IMAGE" >/dev/null
docker image inspect --format '{{.Id}}' "$POSTGRES_IMAGE" | grep -E '^sha256:[a-f0-9]{64}$' >/dev/null
docker save --output "$TMP/postgres-image.tar" "$POSTGRES_IMAGE"
docker run --rm --user "$TRIVY_USER" -v "$TMP:/scan:ro" -v "$TRIVY_CACHE:/tmp/trivy-cache" "$TRIVY_IMAGE" image --cache-dir /tmp/trivy-cache --input /scan/postgres-image.tar --scanners vuln --severity HIGH,CRITICAL --exit-code 1 --no-progress

cat > "$SOURCE/negative-privileged.yaml" <<'EOF'
apiVersion: v1
kind: Pod
metadata: { name: negative-privileged }
spec:
  containers:
    - name: privileged
      image: busybox:1.36
      securityContext: { privileged: true }
EOF
if docker run --rm --user "$TRIVY_USER" -v "$SOURCE:/repo:ro" -v "$TRIVY_CACHE:/tmp/trivy-cache" "$TRIVY_IMAGE" config --cache-dir /tmp/trivy-cache --severity HIGH,CRITICAL --exit-code 1 /repo/negative-privileged.yaml >"$TMP/trivy-negative.log" 2>&1; then
  echo "privileged IaC mutation was not rejected" >&2
  exit 1
fi
if ! grep -q 'negative-privileged.yaml' "$TMP/trivy-negative.log" || ! grep -q 'KSV-0017' "$TMP/trivy-negative.log"; then
  sed -n '1,160p' "$TMP/trivy-negative.log" >&2
  echo "privileged IaC mutation failed without the expected KSV-0017 finding" >&2
  exit 1
fi
rm -f "$SOURCE/negative-privileged.yaml"
echo "negative mutation passed: privileged IaC was rejected"

docker save --output "$TMP/web-image.tar" observability-dashboard-web:ci
docker save --output "$TMP/api-image.tar" observability-dashboard-api:ci
docker save --output "$TMP/web-builder-image.tar" observability-dashboard-web-builder:ci
docker run --rm --user "$TRIVY_USER" -v "$TMP:/scan:ro" -v "$TRIVY_CACHE:/tmp/trivy-cache" "$TRIVY_IMAGE" image --cache-dir /tmp/trivy-cache --input /scan/web-builder-image.tar --severity HIGH,CRITICAL --exit-code 1 --no-progress
docker run --rm --user "$TRIVY_USER" -v "$TMP:/scan:ro" -v "$TRIVY_CACHE:/tmp/trivy-cache" "$TRIVY_IMAGE" image --cache-dir /tmp/trivy-cache --input /scan/web-image.tar --severity HIGH,CRITICAL --exit-code 1 --no-progress
docker run --rm --user "$TRIVY_USER" -v "$TMP:/scan:ro" -v "$TRIVY_CACHE:/tmp/trivy-cache" "$TRIVY_IMAGE" image --cache-dir /tmp/trivy-cache --input /scan/api-image.tar --severity HIGH,CRITICAL --exit-code 1 --no-progress
TELEMETRY_TRIVY_CACHE="$TRIVY_CACHE" TELEMETRY_TRIVY_CACHE_ROOT="$TMP" \
  sh "$ROOT/deploy/scripts/check-telemetry-images.sh"
