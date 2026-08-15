# ADR 0012 — Alertmanager 현재 알림과 해소 이력의 경계를 분리한다

- 상태: Proposed
- 날짜: 2026-08-15
- 관련: 이슈 #1, #17 · ADR 0002, 0005, 0007, 0010

## 배경

Alertmanager API v2의 `GET /api/v2/alerts`는 현재 firing/suppressed 알림의 스냅숏이다.
`active=false`는 해소 이력이 아니며, 프로세스 메모리의 이전 poll이나 webhook으로 이력을
지어내면 replica·재시작에 따라 서로 다른 결과를 만든다.

## 결정

- BFF는 Alertmanager를 HTTPS·Bearer token file·private CA로 조회하는 읽기 전용 adapter만 둔다.
- API v2의 `active`, `suppressed`, `unprocessed` 상태는 모두 현재 `firing`으로 정규화하며,
  과거 또는 이미 끝난 alert는 현재 목록으로 받아들이지 않는다.
- 서버가 cluster/namespace matcher를 삽입하고 응답을 다시 검증한다. 신원은 informer/Registry
  catalog의 Pod UID, 그다음 Workload UID로만 연결한다.
- 현재 firing 조회가 성공해도 Resolved와 resolved 포함 counts는 독립적으로
  `history_not_configured` degraded가 된다.
- Resolved 운영 이력은 명시적인 Loki 또는 `GRAFANA_ALERTS` history adapter를 채택할 때까지
  P2 후속이다. 새 evaluator, webhook 저장소, 요청/프로세스 로컬 이력은 만들지 않는다.

## 검토한 대안

| 대안 | 결정 |
|---|---|
| `active=false`를 resolved로 해석 | API 의미가 달라 기각 |
| 이전 poll을 프로세스 메모리에 보존 | replica·재시작 정합성이 없어 기각 |
| Alertmanager와 함께 evaluator/webhook DB 구현 | 기존 알림 계층을 중복하므로 비목표 |
| Loki/Grafana history adapter | P2 후속. 별도 운영 계약과 보존 정책이 필요 |

## 결과

Alertmanager 장애는 전체 readiness를 내리지 않고 알림 섹션만 degraded로 만든다. 현재 알림과
SSE diff는 실소스를 쓰지만, Resolved 탭은 history adapter가 없음을 명시한다. Helm 기본값은
`alerts.enabled=false`이며 활성화 시에만 Secret projection과 제한된 egress가 생긴다.
