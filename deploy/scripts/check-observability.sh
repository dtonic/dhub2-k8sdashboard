#!/usr/bin/env sh
set -eu
ROOT=$(CDPATH= cd -- "$(dirname "$0")/../.." && pwd)
python3 "$ROOT/scripts/check-observability.py" --self-test
PROMTOOL_IMAGE='prom/prometheus:v3.5.0@sha256:8672a850efe2f9874702406c8318704edb363587f8c2ca88586b4c8fdb5cea24'
docker run --rm --entrypoint /bin/promtool -v "$ROOT/deploy/monitoring:/monitoring:ro" "$PROMTOOL_IMAGE" \
  check rules /monitoring/alerts.yaml
