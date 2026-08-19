# Architecture Decision Records

되돌리기 비싼 결정만 기록합니다.

## 작성 방식 (2026-08-19 변경)

- **0001~0013은 이 디렉터리에 파일**(`NNNN-kebab-case-title.md`)로 있습니다. 번호는 재사용하지 않습니다.
- **0014부터 새 ADR은 GitHub Issue로 만듭니다.** 제목 맨 앞에 `[ADR]`, `ADR` 라벨을 답니다.
  본문에 ADR 번호·상태·배경·검토한 대안·결정·결과·롤백을 적습니다.
  - 목록: `gh issue list --label ADR` · 생성 라벨이 없으면 `gh label create ADR`.
  - 파일 ADR을 대체·갱신할 때도 새 ADR 이슈를 만들고 아래 표의 상태를 갱신합니다.

## 목록 (파일 ADR)

| # | 제목 | 상태 | 날짜 |
|---|---|---|---|
| [0001](./0001-design-system-with-claude-design.md) | 디자인 시스템을 `design-system/`에서 관리하고 Claude Design으로 동기화한다 | Accepted | 2026-08-13 |
| [0002](./0002-screen-scoped-aggregated-endpoints.md) | 화면 단위 집계 엔드포인트를 BFF의 기본 형태로 삼는다 | Accepted | 2026-08-13 |
| [0003](./0003-log-cursor-paging-and-server-side-masking.md) | 로그는 커서로 페이징하고, 마스킹은 서버에서만 한다 | Superseded by 0006 | 2026-08-13 |
| [0004](./0004-backend-language-go.md) | Observability API/BFF는 Go로 구현한다 | Accepted | 2026-08-13 |
| [0005](./0005-mvp-hybrid-architecture.md) | MVP는 기존 관측 스택과 전용 BFF/UI를 결합한 하이브리드 아키텍처로 구성한다 | Accepted | 2026-08-14 |
| [0006](./0006-quickwit-scroll-cursor.md) | Quickwit 로그 페이징에 TTL scroll capability를 사용한다 | Accepted | 2026-08-14 |
| [0007](./0007-sse-replay-reset-backpressure.md) | SSE 상태 변경 스트림은 프로세스 로컬 재생 링 · reset 폴백 · 연결 절단 backpressure로 유계를 유지한다 | Accepted | 2026-08-14 |
| [0008](./0008-opentelemetry-agent-gateway-pipeline.md) | OpenTelemetry Agent/Gateway 수집 소유권과 fail-closed cutover를 표준화한다 | Proposed | 2026-08-15 |
| [0009](./0009-dashboard-drafts-postgresql-and-git-export.md) | Dashboard draft PostgreSQL 저장과 승인본 Git export | Proposed (0016으로 확장) | 2026-08-15 |
| [0010](./0010-multi-cluster-state-agent-registry.md) | 멀티 클러스터 상태 Agent와 중앙 Registry | Proposed | 2026-08-15 |
| [0011](./0011-server-side-oidc-browser-session.md) | 서버 측 OIDC 브라우저 세션 | Proposed | 2026-08-15 |
| [0012](./0012-alertmanager-current-alerts-and-history-boundary.md) | Alertmanager 현재 알림과 해소 이력의 경계를 분리한다 | Proposed | 2026-08-15 |
| [0013](./0013-dhub2-ansible-role-deployment.md) | dhub2 Ansible role로 대시보드 배포와 모니터링 연결을 선언적으로 자동화한다 | Proposed | 2026-08-18 |

## 목록 (이슈 ADR, 0014~)

| # | 제목 | 상태 | 이슈 |
|---|---|---|---|
| 0014 | 관리자 전용 Deployment/Secret 관리(쓰기·Secret 노출)를 조회 경로와 격리해 추가한다 | Proposed | [#34](https://github.com/xenx96/k8s-dashboard/issues/34) |
| 0015 | Dashboard Builder 위젯 확장·예시 대시보드 갱신·편집 UX 개선 | Accepted | [#35](https://github.com/xenx96/k8s-dashboard/issues/35) |
| 0016 | Dashboard draft 저장소를 SQLite 파일(PVC)로 선택 지원하고 Import를 추가한다 (0009 확장) | Proposed | [#37](https://github.com/xenx96/k8s-dashboard/issues/37) |

## 상태 값

- **Proposed** — 논의 중
- **Accepted** — 확정. 코드가 이 결정을 따릅니다.
- **Superseded by NNNN** — 다른 ADR로 대체됨. 원문은 지우지 않고 남겨둡니다.
- **Deprecated** — 더 이상 유효하지 않으나 대체 결정이 없음

## 템플릿

```markdown
# ADR NNNN — 제목

- 상태: Proposed
- 날짜: YYYY-MM-DD
- 결정자:
- 관련:

## 배경
어떤 힘(제약, 요구사항, 문제)이 결정을 강제하는가.

## 결정
무엇을 하기로 했는가. 단정형으로 쓴다.

## 검토한 대안
표로. 각 대안의 기각 사유를 남긴다.

## 결과
좋아지는 것 / 감수하는 것을 모두 쓴다.

## 후속 작업
```
