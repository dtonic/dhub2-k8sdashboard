# CLAUDE.md

이 저장소에서 작업할 때 Claude가 따라야 할 규칙입니다.

## 프로젝트

Kubernetes 운영자용 커스텀 Observability Dashboard입니다. 기존 수집·저장 계층
(GreptimeDB 메트릭 / Quickwit 로그 / Kubernetes API / Grafana Alerting)은 유지하고,
그 위에 **Go 기반 Observability API·BFF**와 **React/TypeScript UI**를 얹습니다.

Grafana를 대체하는 범용 시각화 도구를 만드는 것이 **아닙니다.** 운영 흐름
(장애 발견 → Namespace/Workload 확인 → Pod/Container 확인 → Metric/Log/Event/Alert 상관분석)에
특화된 UX와 통합 조회 계층을 추가하는 것이 목표입니다.

현재 단계: `design-system/`, `apps/web`의 **MVP UI 화면 전체**(Cluster Overview,
Namespace/Workload/Pod Drill-down, Logs Explorer, Pod Topology, Alerts)와
`apps/api`의 **Go Observability API/BFF**가 구현되어 있습니다.
GreptimeDB·Quickwit 실클라이언트는 구현되어 있습니다
(`apps/api/internal/datasource/greptime`, `quickwit` — `GREPTIME_URL`·`QUICKWIT_URL`로 활성화).
남은 것은 Alertmanager 실클라이언트(#17 잔여), 등록형 Query Catalog(#9),
OIDC/RBAC 연결(#10), Redis 캐시(#11), SSE(#12)입니다.
전체 맥락은 `README.md`, 확정된 결정은 `docs/adr/`,
프런트엔드 규칙은 `apps/web/README.md`, 백엔드 규칙은 `apps/api/README.md`에 있습니다.

작업 단위는 GitHub Issues를 따릅니다. 화면·기능 작업을 시작하기 전에 해당 이슈의
**작업 범위와 완료 기준**을 먼저 읽고, 완료 기준을 만족했는지 실제로 확인한 뒤 끝냅니다.

## 시작 전에

1. 원격 변경사항을 먼저 확인합니다. 변경이 있으면 `git fetch && git pull` 후에 작업합니다.
2. 관련 ADR(`docs/adr/`)을 읽습니다. ADR과 충돌하는 구현을 제안하지 않습니다.
   결정을 바꿔야 한다면 코드를 먼저 고치지 말고 **새 ADR을 제안**합니다.

## 반드시 지킬 것

### 보안 (README §10)

- UI가 보낸 Cluster/Namespace 값을 신뢰하지 않습니다. 권한 Scope는 **서버에서 강제 삽입**합니다.
- 데이터소스 Credential을 브라우저에 노출하지 않습니다. GreptimeDB · Quickwit · Kubernetes API
  접근은 서버에서만 수행합니다.
- 프런트엔드에서 Raw PromQL/SQL/Quickwit Query를 전달하지 않습니다. 서버에 등록된 `queryRef`를 씁니다.
- 사용자 권한 Scope를 캐시 키에 포함합니다. 로그의 Token/Password/Secret은 마스킹합니다.
- Secret을 git에 커밋하지 않습니다.

### 성능 (README §11)

- 브라우저에 대량 시계열 포인트를 그대로 보내지 않습니다. 조회 범위에 따라 Step과 Downsampling을 조정합니다.
- 로그는 Cursor/Search-after 방식으로 조회합니다. **offset 페이징을 만들지 않습니다** (ADR 0003).
- **로그 마스킹은 서버에서만 합니다.** 원문을 브라우저로 내려보내고 UI에서 가리는 구현을 하지 않습니다.
  UI는 가려졌다는 사실을 보이게 할 뿐이고, 원문 조회·복사 경로를 만들지 않습니다.
- 요청 취소를 데이터소스 요청까지 전파합니다. 데이터소스별 Timeout과 Circuit Breaker를 적용합니다.

### 데이터 접근 (ADR 0002, `apps/web/README.md`)

- **화면 하나 = 요청 하나.** 위젯마다 API를 만들지 않습니다. 화면 단위 집계 엔드포인트를 씁니다.
- 부분 장애는 예외가 아니라 값입니다. `Section<T>` 봉투로 표현하고, 한 데이터소스가 죽어도
  나머지 패널은 값을 유지합니다.
- **"데이터 없음 · 권한 없음 · upstream 장애"를 같은 빈 화면으로 처리하지 않습니다.**
- 브라우저에서 데이터소스를 직접 호출하지 않습니다. `apps/web/src/api/client.ts`가 유일한 통로입니다.
- 자동 갱신은 데이터만 바꿉니다. 페이지를 다시 마운트하지 않습니다.
- **사용자 상태를 갱신이 지우지 않습니다.** 필터·선택은 URL에, 스크롤은 DOM에 둡니다.
- **Pod의 신원은 이름이 아니라 UID입니다.** 캐시 키와 목록 key에 UID를 씁니다.
- 큰 목록은 가상 스크롤을 씁니다. 전체를 한 번에 DOM에 그리지 않습니다.
- **알림 화면은 조회 전용입니다.** Rule 편집·Silence·Routing 변경 UI를 만들지 않습니다.
- mock에서 Pod 이름·UID를 새로 지어내지 않습니다. `mocks/drilldown.ts`의 `primaryPod()`를 씁니다.
  백엔드도 같습니다 — 데이터소스 어댑터는 `clusterstate.Store.CatalogPods()`에서 신원을 빌려옵니다.
  각자 신원을 만들면 화면 간 deep link가 404가 됩니다.

### 백엔드 (ADR 0004, `apps/api/README.md`)

- **요청 처리 경로에서 Kubernetes API를 호출하지 않습니다.** 항상 informer 캐시(lister)에서 읽습니다.
  요청당 API 서버 호출은 0회여야 하며, `TestQueriesNeverCallTheAPIServer`가 이를 셉니다.
- **폴링을 만들지 않습니다.** 자동 갱신은 브라우저 → 백엔드까지이고, 백엔드 → API 서버는 watch 하나입니다.
- resync를 짧게 두지 않습니다(기본 10분). 쓰지 않는 리소스에 informer를 붙이지 않습니다.
- 관계만 필요한 리소스(ReplicaSet)는 **metadata-only informer**를 씁니다.
- 내장 타입은 **protobuf로 협상**합니다. 이 설정을 끄는 변경은 ADR 0004의 1순위 기준을 되돌립니다.
- Event는 전부 watch하지 않습니다. 기본 필드 셀렉터는 `type=Warning`입니다.
- 섹션 degraded 사유에 내부 주소·질의·스택트레이스를 담지 않습니다.
- Scope는 `scope.Resolver` 뒤에서만 정합니다. 핸들러가 요청 파라미터로 권한을 판단하지 않습니다.
- informer 대상 리소스나 콘텐츠 협상을 바꾸면 **`make api-itest`로 실서버에서 재확인**합니다.
  fake clientset은 protobuf 협상도 필드 셀렉터도 검증하지 못합니다.
- 실클러스터 통합 테스트는 **기본이 읽기 전용**입니다. 객체를 만드는 테스트는
  `ITEST_MUTATE=1`로만 돌게 두고, 기본 경로에서 클러스터를 건드리지 않습니다.

### 엔티티 식별 (README §5)

- Pod 이름보다 **Pod UID · Workload UID**를 우선합니다.
  식별 우선순위: Pod UID → Workload UID → Namespace + Kind + Name → Pod Name.

### 디자인 (ADR 0001, `design-system/README.md`)

UI 작업 시 **`design-system/README.md`를 먼저 읽습니다.**

- 원시 hex나 임의 픽셀값을 쓰지 않습니다. `design-system/tokens/`의 역할 토큰만 참조합니다.
- Light/Dark는 자동 반전이 아닙니다. 두 모드 모두에서 확인합니다.
- 상태 색(`good/warning/serious/critical`)은 예약어입니다. 차트 계열 색으로 재사용하지 않고,
  항상 아이콘 + 텍스트 라벨과 함께 씁니다.
- 차트: **이중 y축 금지**, 계열 색은 고정 순서 배정(순환 금지), 색은 엔티티에 고정,
  2계열 이상이면 범례 필수, 텍스트에 계열 색을 입히지 않음.
- 차트 계열 색을 바꾸면 `dataviz` validator를 Light/Dark 두 모드로 재실행하고
  `design-system/README.md`의 검증 기록 표를 갱신합니다.
- 토폴로지: `A→B`와 `B→A`는 **별도의 선**입니다. 하나의 선에 양방향 화살표를 달지 않습니다.
  선 색은 상태, 두께는 트래픽 양이며 프로토콜은 텍스트로 구분합니다.
- 시계열 Step은 사용자가 고르지 않습니다. 범위에 따라 서버가 강제하고 UI는 표시만 합니다
  (1시간→1분, 1일→5분, 7일→15분, 30일→1시간). Custom Range 최대 30일.
- 새 컴포넌트를 추가하면 `*.preview.html`도 함께 만듭니다. 첫 줄에 `@dsCard` 마커가 필요합니다.
  작업 후 `cd design-system && npm run check`가 통과해야 합니다.

## 하지 말 것

- Grafana 기능을 통째로 재현하려는 시도 (Raw Query Editor, 자체 Alert Rule Evaluator,
  Silence/Grouping/Notification Router, 완전한 Dashboard Builder는 MVP 제외 범위입니다)
- 수집 파이프라인 교체 (OpenTelemetry 표준화는 Post-MVP입니다)
- 요청받지 않은 커밋·푸시
- 검증 없이 차트 색상 변경
- `Lorem ipsum` 같은 더미 문구 — preview에는 실제 리소스 이름과 실제 문구를 씁니다

## 작성 언어

문서와 주석은 한국어, 코드 식별자는 영어로 씁니다. Go 코드도 같습니다 — 주석은 한국어로 쓰되
`godoc` 관례대로 선언 이름으로 시작합니다. 커밋 메시지는 Conventional Commits
(`feat:`, `fix:`, `docs:`, `chore:`) 형식의 영어 제목을 사용합니다.
