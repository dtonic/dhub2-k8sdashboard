#!/usr/bin/env sh
set -eu

ROOT=$(CDPATH= cd -- "$(dirname "$0")/../.." && pwd)
CHART="$ROOT/deploy/helm/observability-dashboard"
TMP_BASE=$(realpath "${TMPDIR:-/tmp}")
TMP=$(mktemp -d "$TMP_BASE/dashboard-deploy.XXXXXX")
cleanup() {
  resolved=$(realpath "$TMP" 2>/dev/null || true)
  case "$resolved" in
    "$TMP_BASE"/dashboard-deploy.*)
      if [ -n "${COLLECTOR_IMAGE_ID:-}" ] && [ "$(docker image inspect -f '{{.Id}}' "$COLLECTOR_IMAGE" 2>/dev/null || true)" = "$COLLECTOR_IMAGE_ID" ]; then
        docker image rm "$COLLECTOR_IMAGE" >/dev/null
      fi
      [ ! -d "$resolved" ] || rm -rf -- "$resolved"
      ;;
    *) echo "refusing to clean unexpected deploy temp path: $resolved" >&2; return 1 ;;
  esac
}
trap cleanup EXIT HUP INT TERM

HELM_IMAGE='alpine/helm:3.17.3@sha256:d899e6316789fec04ee95300a18e454b7942539cbb3d89bde3e0655d6ca2e895'
KUBECONFORM_IMAGE='ghcr.io/yannh/kubeconform:v0.6.7@sha256:0925177fb05b44ce18574076141b5c3d83235e1904d3f952182ac99ddc45762c'
COLLECTOR_TOKEN=$(printf '%s' "${TMP##*.}" | tr '[:upper:]' '[:lower:]')
COLLECTOR_IMAGE="issue23-otelcol-$COLLECTOR_TOKEN:ci"
export TELEMETRY_COLLECTOR_IMAGE="$COLLECTOR_IMAGE"

sh "$ROOT/deploy/scripts/build-telemetry-collector.sh"
COLLECTOR_IMAGE_ID=$(docker image inspect -f '{{.Id}}' "$COLLECTOR_IMAGE")

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

# The default is an exact opt-out: explicitly setting disabled must not alter the
# existing development manifest. Validate mode is backend-write-free; cutover is
# checked against the complete deterministic fixture.
docker run --rm -v "$ROOT:/work:ro" "$HELM_IMAGE" template release-name /work/deploy/helm/observability-dashboard \
  --namespace observability --values /work/deploy/helm/observability-dashboard/values-dev.yaml \
  --set telemetry.mode=disabled > "$TMP/disabled-explicit.yaml"
cmp "$TMP/dev.yaml" "$TMP/disabled-explicit.yaml"
for env in dev stage prod; do
  docker run --rm -v "$ROOT:/work:ro" "$HELM_IMAGE" template release-name /work/deploy/helm/observability-dashboard \
    --namespace observability --values "/work/deploy/helm/observability-dashboard/values-$env.yaml" \
    --set dashboardBuilder.enabled=false > "$TMP/$env-builder-disabled.yaml"
  cmp "$TMP/$env.yaml" "$TMP/$env-builder-disabled.yaml"
done

docker run --rm -v "$ROOT:/work:ro" "$HELM_IMAGE" template release-name /work/deploy/helm/observability-dashboard \
  --namespace observability --values /work/deploy/helm/observability-dashboard/values-dev.yaml \
  --set dashboardBuilder.enabled=true --set api.existingSecret.name=issue24-fixture \
  --set 'dashboardBuilder.postgresEgress.cidrs[0]=192.0.2.30/32' > "$TMP/dashboard-builder-enabled.yaml"
python3 - "$TMP/dashboard-builder-enabled.yaml" <<'PY'
import sys, yaml
docs=[d for d in yaml.safe_load_all(open(sys.argv[1])) if d]
deploy=next(d for d in docs if d.get("kind")=="Deployment" and d["metadata"]["name"].endswith("-api"))
env={e["name"]:e for e in deploy["spec"]["template"]["spec"]["containers"][0]["env"]}
for name,key in (("DATABASE_URL","DATABASE_URL"),("DASHBOARD_CURSOR_KEY","DASHBOARD_CURSOR_KEY")):
    ref=env[name]["valueFrom"]["secretKeyRef"]
    assert ref=={"name":"issue24-fixture","key":key,"optional":False},ref
assert env["DASHBOARD_DB_MAX_CONNS"]["value"]=="8"
assert env["DASHBOARD_DB_CONNECT_TIMEOUT"]["value"]=="5s"
assert env["DASHBOARD_DB_REQUIRE_TLS"]["value"]=="false"
policy=next(d for d in docs if d.get("kind")=="NetworkPolicy" and d["metadata"]["name"].endswith("-api"))
assert any(e.get("to")==[{"ipBlock":{"cidr":"192.0.2.30/32"}}] and e.get("ports")==[{"protocol":"TCP","port":5432}] for e in policy["spec"]["egress"])
text=open(sys.argv[1]).read();assert "postgres://" not in text and "issue24-test-only" not in text
PY

docker run --rm -v "$ROOT:/work:ro" "$HELM_IMAGE" template release-name /work/deploy/helm/observability-dashboard \
  --namespace observability --values /work/deploy/helm/observability-dashboard/values-dev.yaml \
  --set telemetry.mode=validate --set telemetry.clusterName=fixture-cluster \
  --set telemetry.collectorBuildVerified=true \
  --set telemetry.image.repository=registry.local/observability-dashboard-otelcol \
  --set telemetry.image.digest=sha256:0000000000000000000000000000000000000000000000000000000000000000 \
  > "$TMP/telemetry-validate.yaml"
docker run --rm -v "$ROOT:/work:ro" "$HELM_IMAGE" template release-name /work/deploy/helm/observability-dashboard \
  --namespace observability --values /work/deploy/helm/observability-dashboard/values-dev.yaml \
  --values /work/deploy/telemetry/fixtures/cutover-local.yaml > "$TMP/telemetry-cutover.yaml"
for manifest in telemetry-validate telemetry-cutover; do
  docker run --rm -i "$KUBECONFORM_IMAGE" -strict -summary -kubernetes-version 1.31.0 < "$TMP/$manifest.yaml"
  python3 "$ROOT/scripts/check-deploy.py" "$TMP/$manifest.yaml" --environment dev --self-test
  for component in agent gateway cluster; do
    python3 "$ROOT/deploy/scripts/extract-collector-config.py" "$TMP/$manifest.yaml" \
      --suffix="-otel-$component" --output "$TMP/$manifest-$component.yaml"
  done
done

! grep -q 'State\.' "$ROOT/deploy/scripts/test-telemetry-protocol.py"
PYTHONDONTWRITEBYTECODE=1 python3 -B "$ROOT/deploy/scripts/test-telemetry-protocol.py" \
  --evidence-out "$TMP/telemetry-evidence.json"
PYTHONDONTWRITEBYTECODE=1 python3 -B "$ROOT/deploy/scripts/check-telemetry-evidence.py" \
  "$TMP/telemetry-evidence.json" --environment local
PYTHONDONTWRITEBYTECODE=1 python3 -B - "$TMP/telemetry-evidence.json" "$TMP" <<'PY'
import copy, hashlib, json, re, sys
from datetime import datetime, timedelta, timezone
from pathlib import Path

source = json.loads(Path(sys.argv[1]).read_text())
root = Path(sys.argv[2])
def write(name, mutate, rehash=True):
    value = copy.deepcopy(source)
    mutate(value)
    if rehash:
        unsigned = {key: item for key, item in value.items() if key != "artifactHash"}
        value["artifactHash"] = hashlib.sha256(json.dumps(unsigned, sort_keys=True, separators=(",", ":")).encode()).hexdigest()
    (root / f"evidence-negative-{name}.json").write_text(json.dumps(value))
write("unknown", lambda value: value.update({"unknown": 1}))
write("loss", lambda value: value.update({"lossPermille": 1}))
write("kind", lambda value: value.update({"kind": "production-comparison"}))
write("bool-window", lambda value: value.update({"windowMinutes": True}))
write("string-count", lambda value: value["raw"].update({"baselineEvents": "30"}))
write("float-loss", lambda value: value.update({"lossPermille": 0.0}))
write("operator-flag", lambda value: value.update({"operatorProductionMeasurementsRequired": False}))
def stale(value):
    ended = datetime.now(timezone.utc) - timedelta(days=1)
    value["endedAt"] = ended.isoformat().replace("+00:00", "Z")
    value["startedAt"] = (ended - timedelta(minutes=1)).isoformat().replace("+00:00", "Z")
stale_value = stale
write("stale", stale_value)
write("tampered-hash", lambda value: value["raw"].update({"payloadBytes": value["raw"]["payloadBytes"] + 1}), rehash=False)
write("topology", lambda value: value["raw"].update({"candidateTopology": "bypass"}))
write("corpus-digest", lambda value: value["raw"].update({"corpusDigest": "0" * 64}))
write("corpus-event", lambda value: value["raw"]["corpusEventDigests"].__setitem__(0, "0" * 64))
write("latency", lambda value: value.update({"p95LatencyMs": value["p95LatencyMs"] + 1}))
write("trial-latency", lambda value: value["raw"]["candidateTrialP95Ms"].__setitem__(0, value["raw"]["candidateTrialP95Ms"][0] + 1))
write("measurement-duration", lambda value: value["raw"].update({"candidateMeasurementDurationMs": value["raw"]["candidateMeasurementDurationMs"] + 10_000}))
write("cpu-duration", lambda value: value["raw"]["cpuTrialDurationMs"].__setitem__(0, value["raw"]["cpuTrialDurationMs"][0] + 10_000))
write("resource", lambda value: value.update({"collectorMemoryMiB": value["collectorMemoryMiB"] + 1}))
write("resource-name", lambda value: value["raw"]["candidateCollectorSamples"][0][0].update({"name": "unknown"}))
write("baseline-resource", lambda value: value["raw"].update({"baselineMemoryMiB": value["raw"]["baselineMemoryMiB"] + 1}))
write("cpu-time", lambda value: value["raw"]["candidateCpuTimeNanos"].__setitem__(0, value["raw"]["candidateCpuTimeNanos"][0] + 1))
write("stored", lambda value: value["raw"]["storedBytes"].update({"quickwit": value["raw"]["storedBytes"]["quickwit"] + 1}))
write("egress", lambda value: value.update({"egressBytesPerHour": value["egressBytesPerHour"] + 1}))
write("cost", lambda value: value.update({"estimatedCostMicrosPerDay": value["estimatedCostMicrosPerDay"] + 1}))
(root / "evidence-negative-nan.json").write_text(re.sub(r'"cpuPercent": [0-9.]+', '"cpuPercent": NaN', json.dumps(source), count=1))
duplicate = json.dumps(source)
needle = f'"baselineEvents": {source["raw"]["baselineEvents"]}'
(root / "evidence-negative-duplicate-key.json").write_text(duplicate.replace(needle, needle + ", " + needle, 1))
PY
for mutation in unknown loss kind bool-window string-count float-loss operator-flag stale tampered-hash topology corpus-digest corpus-event latency trial-latency measurement-duration cpu-duration resource resource-name baseline-resource cpu-time stored egress cost nan duplicate-key; do
  if PYTHONDONTWRITEBYTECODE=1 python3 -B "$ROOT/deploy/scripts/check-telemetry-evidence.py" \
    "$TMP/evidence-negative-$mutation.json" --environment local >/dev/null 2>&1; then
    echo "telemetry evidence negative mutation unexpectedly passed: $mutation" >&2
    exit 1
  fi
done

! grep -q 'otlp_http/greptime\|otlp_grpc/quickwit\|GREPTIME_OTLP_AUTHORIZATION\|QUICKWIT_OTLP_AUTHORIZATION' "$TMP/telemetry-validate-gateway.yaml"
grep -q 'exporters: \[nop\]' "$TMP/telemetry-validate-gateway.yaml"
grep -q 'otlp_http/greptime' "$TMP/telemetry-cutover-gateway.yaml"
grep -q 'x-greptime-otlp-metric-translation-strategy: NoTranslation' "$TMP/telemetry-cutover-gateway.yaml"
grep -q 'otlp_grpc/quickwit' "$TMP/telemetry-cutover-gateway.yaml"
grep -q 'qw-otel-logs-index:.*otel-logs-v0_7' "$TMP/telemetry-cutover-gateway.yaml"
grep -q 'QUICKWIT_INDEX:.*otel-logs-v0_7' "$TMP/telemetry-cutover.yaml"
grep -q 'QUICKWIT_FIELDS: "timestamp=timestamp_nanos,level=severity_text,message=body.message' "$TMP/telemetry-cutover.yaml"
! grep -q '^kind: Secret$' "$TMP/telemetry-cutover.yaml"

mkdir -p "$TMP/hostfs" "$TMP/serviceaccount"
: > "$TMP/serviceaccount/token"
: > "$TMP/serviceaccount/ca.crt"
docker run --rm "$COLLECTOR_IMAGE" components > "$TMP/collector-components.txt"
for component in file_log host_metrics prometheus k8s_cluster otlp k8s_attributes memory_limiter filter transform batch nop otlp_grpc otlp_http; do
  grep -q "$component" "$TMP/collector-components.txt"
done
for manifest in telemetry-validate telemetry-cutover; do
  docker run --rm -e K8S_NODE_IP=127.0.0.1 \
    -v "$TMP/$manifest-agent.yaml:/conf/config.yaml:ro" -v "$TMP/hostfs:/hostfs:ro" \
    -v "$TMP/serviceaccount:/var/run/secrets/kubernetes.io/serviceaccount:ro" \
    "$COLLECTOR_IMAGE" validate --config=/conf/config.yaml
  docker run --rm -e GREPTIME_OTLP_AUTHORIZATION=fixture -e QUICKWIT_OTLP_AUTHORIZATION=fixture \
    -v "$TMP/$manifest-gateway.yaml:/conf/config.yaml:ro" \
    "$COLLECTOR_IMAGE" validate --config=/conf/config.yaml
  docker run --rm -v "$TMP/$manifest-cluster.yaml:/conf/config.yaml:ro" \
    "$COLLECTOR_IMAGE" validate --config=/conf/config.yaml
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
expect_render_failure --set dashboardBuilder.enabled=true
expect_render_failure --set dashboardBuilder.enabled=true --set api.existingSecret.name=issue24 --set-json 'dashboardBuilder.postgresEgress.cidrs=[]'
expect_render_failure --set dashboardBuilder.enabled=true --set api.existingSecret.name=issue24 --set 'dashboardBuilder.postgresEgress.cidrs[0]=not-a-cidr'
expect_render_failure --set dashboardBuilder.enabled=true --set api.existingSecret.name=issue24 --set 'dashboardBuilder.postgresEgress.cidrs[0]=192.0.2.30/32' --set dashboardBuilder.postgresEgress.port=0
expect_render_failure --set dashboardBuilder.enabled=true --set api.existingSecret.name=issue24 --set 'dashboardBuilder.postgresEgress.cidrs[0]=192.0.2.30/32' --set dashboardBuilder.maxConnections=33
expect_render_failure --set dashboardBuilder.enabled=true --set api.existingSecret.name=issue24 --set 'dashboardBuilder.postgresEgress.cidrs[0]=192.0.2.30/32' --set dashboardBuilder.connectTimeout=0s
expect_render_failure --set dashboardBuilder.enabled=true --set api.existingSecret.name=issue24 --set 'dashboardBuilder.postgresEgress.cidrs[0]=192.0.2.30/32' --set dashboardBuilder.connectTimeout=31s
if docker run --rm -v "$ROOT:/work:ro" "$HELM_IMAGE" template release-name /work/deploy/helm/observability-dashboard --namespace observability \
  --values /work/deploy/helm/observability-dashboard/values-stage.yaml --set dashboardBuilder.enabled=true \
  --set api.existingSecret.name=issue24 --set 'dashboardBuilder.postgresEgress.cidrs[0]=192.0.2.30/32' >/dev/null 2>&1; then
  echo "stage dashboard builder without verified TLS unexpectedly rendered" >&2; exit 1
fi

expect_cutover_failure() {
  if docker run --rm -v "$ROOT:/work:ro" "$HELM_IMAGE" template release-name /work/deploy/helm/observability-dashboard \
    --namespace observability --values /work/deploy/helm/observability-dashboard/values-dev.yaml \
    --values /work/deploy/telemetry/fixtures/cutover-local.yaml "$@" >/dev/null 2>&1; then
    echo "cutover negative values mutation unexpectedly rendered: $*" >&2
    exit 1
  fi
}
expect_cutover_failure --set telemetry.existingLogCollectionDisabled=false
expect_cutover_failure --set telemetry.existingMetricCollectionDisabled=false
expect_cutover_failure --set telemetry.existingSecret.name=
expect_cutover_failure --set telemetry.backends.greptime.endpoint=
expect_cutover_failure --set telemetry.backends.quickwit.endpoint=
expect_cutover_failure --set telemetry.backends.quickwit.index=custom-logs
expect_cutover_failure --set telemetry.comparison.recorded=false
expect_cutover_failure --set telemetry.comparison.evidenceId=
expect_cutover_failure --set telemetry.comparison.artifactHash=
expect_cutover_failure --set telemetry.comparison.startedAt=
expect_cutover_failure --set telemetry.comparison.endedAt=
expect_cutover_failure --set telemetry.comparison.kind=production-comparison
expect_cutover_failure --set telemetry.comparison.artifactHash=1111111111111111111111111111111111111111111111111111111111111111
expect_cutover_failure --set telemetry.comparison.lossPermille=1
expect_cutover_failure --set api.config.GREPTIME_URL=
expect_cutover_failure --set api.config.QUICKWIT_URL=
expect_cutover_failure --set api.config.USE_DEMO_DATA=true
expect_cutover_failure --set-json 'telemetry.agent.kubeletCidrs=[]'
expect_cutover_failure --set-json 'telemetry.clusterCollector.kubeStateMetrics.namespaceSelector={}'
expect_cutover_failure --set-json 'telemetry.clusterCollector.kubeStateMetrics.podSelector={}'
expect_cutover_failure --set-json 'telemetry.backends.egress=[{"cidr":"192.0.2.22/32","port":4000,"purpose":"greptime"}]'
expect_cutover_failure --set telemetry.image.repository=otel/opentelemetry-collector
expect_cutover_failure --set telemetry.collectorBuildVerified=false
expect_cutover_failure --set telemetry.image.repository=dashboard/otelcol
expect_cutover_failure --set telemetry.image.repository=registry.invalid/dashboard-otelcol
expect_cutover_failure --set telemetry.gateway.pdb.minAvailable=2
expect_cutover_failure --set telemetry.memoryLimiter.spikeLimitMiB=400
expect_cutover_failure --set telemetry.memoryLimiter.limitMiB=512
expect_cutover_failure --set telemetry.agent.resources.limits.memory=1Gi

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
