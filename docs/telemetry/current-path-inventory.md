# Telemetry 현재 경로와 호환성 inventory

## 소유권

| signal/source | 기존 경로 | 새 owner | 중복 방지 |
|---|---|---|---|
| container stdout/stderr | chart 밖 클러스터별 수집기 | node별 Agent `filelog` | logs receiver는 `filelog`만 사용하고 기존 log 수집 중지 승인 필요 |
| kubelet/cAdvisor | 외부 metrics 수집기 | node별 Agent Prometheus receiver | node host IP 한 곳, 네 metric allowlist |
| kube-state-metrics | 외부 Prometheus 계열 | singleton cluster collector | replicas=1, overlap 없는 rolling, 두 metric allowlist |
| cluster telemetry | chart에 없음 | singleton `k8s_cluster` | HA Gateway에서 receiver 미실행 |
| app OTLP | 표준 owner 없음 | 범위 제외 | Service/receiver를 만들지 않음 |
| traces | 범위 제외 | 없음 | pipeline 없음 |

## Query Catalog metric 계약

| metric | type | 보존 label | queryRef |
|---|---|---|---|
| `container_cpu_usage_seconds_total` | counter | `namespace,pod,container` | `metrics.cpu.used`, `metrics.usage.cpu_milli` |
| `container_memory_working_set_bytes` | gauge | `namespace,pod,container` | `metrics.memory.used`, `metrics.usage.memory_mib` |
| `container_network_receive_bytes_total` | counter | `namespace,pod,container,interface` | `metrics.network.rx` |
| `container_network_transmit_bytes_total` | counter | `namespace,pod,container,interface` | `metrics.network.tx` |
| `kube_pod_container_resource_requests` | gauge | `namespace,pod,container,resource,unit` | `metrics.cpu.requested`, `metrics.memory.requested` |
| `kube_pod_container_status_restarts_total` | counter | `namespace,pod,container` | `metrics.restarts` |

Greptime exporter는 `NoTranslation`과 `X-Greptime-DB-Name`을 보낸다. cAdvisor/KSM catalog 전용 pipeline은 이름 allowlist를 적용해 정확히 여섯 metric만 전달하고 Prometheus 자체 `up`/`scrape_*` series를 버리며, 별도 `hostmetrics`/`k8s_cluster` pipeline에는 이 filter를 적용하지 않는다. protocol fixture는 여섯 이름의 type/label/drop/no-duplicate와 9 queryRef를 실제 Greptime에서 검사한다. `hostmetrics`/`k8s_cluster` 추가 metrics는 이 여섯 catalog metric proof와 local 저장·비용 측정 범위 밖이며 Query Catalog 호환 근거가 아니다. local synthetic 비교는 source+Gateway OTLP hop만 측정하므로 실제 DaemonSet 수, singleton cluster workload, Kubernetes API 부하를 대표하지 않는다.

## Log 계약

Quickwit index는 `otel-logs-v0_7`로 고정한다. BFF 13 fields는 timestamp, level, `body.message`, namespace, pod name/UID, container, workload kind/name, node, trace/span, event ID다. resolver는 legacy flat exact key를 먼저 읽고 nested JSON을 반복적으로 읽으며 path는 256 bytes/16 segments로 제한한다.

resource allowlist는 cluster/namespace/workload/pod/container/node/service 계열만 유지한다. log attributes는 `event_id`만 남기고 secret/token/JWT/email/card/IP는 Quickwit 전에 치환한다.

## 런타임 제약

- `filelog`는 표준 `/var/log/pods/<namespace>_<pod>_<uid>/<container>/<restart>.log` CRI path와 권한을 전제로 한다.
- file checkpoint는 process-local이다. 재시작 뒤 `start_at:end`이므로 checkpoint 이후 아직 읽지 못한 기존 파일과 collector downtime 동안 쌓인 줄은 유실될 수 있다. 최대 유실 window는 `마지막 성공 checkpoint부터 새 Agent가 file watcher를 시작할 때까지`이며 pod 복구 시간이 길면 시간 상한도 길어진다. restart가 포함된 stage loss 측정으로 허용 여부를 결정한다.
- kubelet 인증서는 node host IP SAN을 포함해야 하며 TLS 검증을 끄지 않는다.
- 표준 NetworkPolicy는 FQDN을 표현하지 못하므로 egress는 운영자가 확인한 CIDR로 제한한다.
