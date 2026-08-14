# OpenTelemetry 수집 전환 runbook

> `cutover-local.yaml` 값은 render 음성검사용 합성 fixture이며 운영 evidence가 아니다. stage/prod는 실제 실행 artifact만 허용한다.

## 사전 확인

1. Quickwit `otel-logs-v0_7` index를 `deploy/telemetry/quickwit-otel-logs-v0_7.yaml`로 미리 만들고 Index API readback에서 timestamp와 attributes/resource_attributes fast JSON schema를 확인한다. auto-create에 의존하지 않는다.
2. Quickwit search URL(7280)과 OTLP gRPC(7281), Greptime query URL과 OTLP base `/v1/otlp`를 구분한다.
3. KSM endpoint/selectors, kubelet node CIDR:10250, Kubernetes API CIDR, backend CIDR/port를 기록한다.
4. 기존 Secret key 존재만 확인하며 값을 values/render에 넣지 않는다.

## validate와 측정

`telemetry.mode=validate`는 Agent/cluster→Gateway를 실행하지만 Gateway는 `nop`만 사용한다. 기존 collector와 같은 30분 이상 window에서 다음을 실제 측정한다.

| 항목 | 값 |
|---|---|
| loss | raw baseline/candidate counts로 `max(0,floor((baseline-candidate)*1000/baseline))` 재계산 |
| latency | source timestamp부터 backend 검색 가능 시점의 p95 |
| resource | 세 collector workload CPU millicores와 RSS MiB |
| volume/cost | egress bytes/hour, storage bytes/day, cost micros/day |

`test-telemetry-protocol.py --evidence-out`은 `comparisonScope=synthetic-otlp-hop`인 local 비교만 생성한다. 같은 hash-verified source collector가 mock으로 직접 가는 baseline과 Gateway transform을 거치는 candidate에 동일 digest의 30-event corpus를 10건씩 3회 보낸다. CPU 정본은 각 trial의 start snapshot이 모두 끝난 직후부터 workload가 끝난 직후까지의 동일 wall-clock 구간과 모든 collector thread의 `/proc/1/task/*/schedstat` runtime ns를 사용한 평균 millicore이며 helper 기동 시간은 분모에서 제외한다. 순차 snapshot의 작은 시작·종료 skew는 baseline보다 두 candidate process에 보수적으로 불리할 수 있는 local 측정 오차다. `/proc/1/stat`과 실제 `CLK_TCK` 변환값은 교차 기록한다. RSS와 순간 CPU%는 trial 직후 Docker stats 진단값일 뿐 CPU gate의 입력이 아니다. 이것은 기존 production collector 비교도, chart 전체 Agent/cluster workload 비용 대표값도 아니다. 처리율은 latency 합이 아니라 실제 `candidateMeasurementDurationMs`로 계산한다. 저장량은 실제 Greptime/Quickwit ingest 전후 `du` delta, 비용은 USD 0.023/GiB-month를 시장 가격이 아닌 명시적 local 가정으로 두고 retention 7일·replication 1로 계산한다.

`check-telemetry-evidence.py`는 full artifact hash, exact keys/types, derived loss/p95/CPU/RSS/egress/storage/cost, freshness와 environment를 재검산한다. stage/prod는 `comparisonScope=production-existing-vs-otel`, 30분 이상 실제 existing-vs-OTel 측정, 완성된 resource/storage/cost만 허용한다. 운영 values에는 이 checker의 `--helm-values-out` 결과만 사용하며 수기 복사한 값은 인정하지 않는다. `validate` 자체는 backend persistence 증명이 아니다.

재시작 시험을 window에 포함한다. process-local filelog checkpoint 때문에 `마지막 checkpoint~새 watcher 시작`의 로그가 유실될 수 있으며 이 loss가 한도를 넘으면 cutover하지 않는다.

## Collector 게시 preflight

```bash
TELEMETRY_COLLECTOR_IMAGE=registry.example.com/observability/dashboard-otelcol:v0.158.0-dashboard.1 \
  sh deploy/scripts/build-telemetry-collector.sh
telemetry_scan_tar=$(mktemp /tmp/dashboard-otelcol.XXXXXX.tar)
trap 'rm -f -- "$telemetry_scan_tar"' EXIT
docker save --output "$telemetry_scan_tar" \
  registry.example.com/observability/dashboard-otelcol:v0.158.0-dashboard.1
docker run --rm --user "$(id -u):$(id -g)" -v "$telemetry_scan_tar:/scan/image.tar:ro" \
  aquasec/trivy:0.74.0@sha256:62b1e65e8869bc4b4c6aa4fa2b21595256c7c2f6018a9d9ad61caf87187c1969 \
  image --input /scan/image.tar --scanners vuln \
  --severity HIGH,CRITICAL --exit-code 1 --no-progress
```

두 명령은 local hash와 pinned Trivy HIGH/CRITICAL 0 gate를 통과해야 한다. 이후 push 권한이 있는 operator만 이미 검증한 exact image ID를 registry에 push하고 `docker image inspect --format '{{json .RepoDigests}}' <image>`로 게시된 repository와 `sha256:` manifest digest를 readback한다. 그 repository를 `telemetry.image.repository`, manifest digest를 `telemetry.image.digest`에 각각 입력한다. 이 변경에서는 외부 push를 실행하지 않았다. local binary hash와 render fixture의 zero/fake digest는 OCI manifest digest가 아니므로 운영 values에 복사하지 않는다.

`deploy/telemetry/Dockerfile.collector`를 build checker로 재현 빌드하고 HIGH/CRITICAL 0을 확인한 뒤 운영 registry에 게시한다. `collectorBuildVerified=true`와 `telemetry.image.repository@digest`에는 실제 게시된 OCI manifest digest만 사용한다. local binary SHA-256이나 render fixture digest는 운영 digest가 아니다.

## cutover

1. `USE_DEMO_DATA=false`, BFF query URLs, backend endpoints와 두 egress를 채운 render를 검사한다.
2. 기존 logs와 metrics collector를 각각 중지한 뒤 두 ownership 승인 값을 true로 바꾼다.
3. `mode=cutover` 적용 후 Greptime 9 queryRef와 Quickwit filter/paging/facets/histogram을 확인한다.

## 장애와 rollback

| 장애 | 영향 | 대응 |
|---|---|---|
| backend 503 | bounded memory queue/retry 뒤 회복하면 한 번 전달; max elapsed 초과 시 loss | collector health와 refused/dropped counters 확인 |
| queue/limiter 포화 | bounded loss | queue 4096, batch 2048, limiter와 pod limit 조정 후 재측정 |
| singleton 재시작 | KSM/cluster metric 공백 | overlap을 피하며 readiness 회복 확인 |
| kubelet IP SAN 없음 | cAdvisor 실패 | 인증서 수정; insecure 우회 금지 |

Rollback은 telemetry를 먼저 `disabled`로 적용해 exporter를 멈추고 기존 collector를 복구한다. Quickwit index는 데이터 보존을 위해 자동 삭제하지 않는다. 일시적 공백은 허용하지만 dual-write는 금지한다.
