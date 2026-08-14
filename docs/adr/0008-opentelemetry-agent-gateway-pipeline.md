# ADR 0008 — OpenTelemetry Agent/Gateway 표준 수집 파이프라인

- 상태: Proposed
- 날짜: 2026-08-15
- 결정자: 미정
- 관련: Issue #23, ADR 0005, Query Catalog #9

## 배경

BFF는 GreptimeDB metrics와 Quickwit logs를 조회하지만 chart가 수집기 소유권·schema·중복 방지·개인정보 제거를 보장하지 않는다. 기존 수집 경로가 있는 상태에서 새 수집기를 바로 켜면 이중 쓰기가 된다. Query Catalog는 cAdvisor와 kube-state-metrics의 여섯 metric 이름을 전제로 하므로 일반 host metrics로 대체할 수도 없다.

## 검토한 대안

| 대안 | 장점 | 단점 |
|---|---|---|
| 기존 클러스터별 수집기 유지 | 변경 위험이 작음 | 소유권·schema·PII·cardinality가 표준화되지 않음 |
| Agent + singleton cluster collector + HA Gateway | node/cluster 소유권이 명확하고 BFF 계약 보존 | RBAC·NetworkPolicy·운영 측정 추가 |
| 관리형 vendor agent | 운영 부담 일부 위임 | vendor 종속, 기존 backend/BFF 계약 변경 |

## 결정

두 번째 대안을 opt-in으로 채택한다. 기본은 `disabled`이며 기존 render를 바꾸지 않는다. `validate`는 Gateway `nop` exporter만 사용한다. `cutover`는 기존 logs/metrics 수집 중지 승인, 실제 비교 evidence, BFF query URL, 기존 Secret과 bounded backend egress가 모두 있어야 활성화된다.

Agent는 CRI logs와 node-local kubelet/cAdvisor를, singleton cluster collector는 KSM과 `k8s_cluster`을 소유한다. 두 구성요소는 Gateway로 한 번만 전송한다. Gateway는 schema/cardinality/PII 처리와 backend export만 담당한다. traces는 제외한다.

Collector는 v0.158.0 component module을 Go 1.26.6으로 재현 빌드한 최소 OCB distribution만 사용한다. upstream contrib v0.158.0 image는 현재 HIGH stdlib 취약점 때문에 직접 실행하지 않는다. 운영 enable은 build hash 검증, HIGH/CRITICAL 0 scan, operator registry publish와 immutable OCI manifest digest 확인 뒤에만 허용한다.

## 결과

| 유형 | 영향 |
|---|---|
| 기술 | cAdvisor/KSM metric 이름을 `NoTranslation`으로 보존하고 Quickwit `otel-logs-v0_7` 13-field 계약을 고정한다. |
| 운영 | CRI 경로, kubelet IP SAN, KSM selector, backend CIDR와 실제 비교 측정이 필요하다. |
| 보안 | 최소 RBAC, default-deny NetworkPolicy, 기존 Secret 참조, export 전 PII 제거를 적용한다. |
| 장기 | metric/schema 확장은 Query Catalog와 protocol fixture를 함께 변경해야 한다. |

## 롤백 고려사항

- 새 pipeline을 먼저 `disabled`로 적용하고 exporter 중지를 확인한 뒤 기존 수집기를 복구한다.
- 일시적 수집 공백을 중복보다 우선하며 두 pipeline을 동시에 켜지 않는다.
- backend에 이미 기록된 데이터는 chart rollback으로 삭제되지 않는다.
- 운영 cutover 전 Proposed ADR 승인과 실제 stage evidence가 필요하다.
