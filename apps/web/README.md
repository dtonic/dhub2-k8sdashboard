# apps/web

인증 UI는 같은 immutable image를 사용하며, 활성 모드는 nginx가 최초 HTML에 삽입하는
비밀이 아닌 meta로 결정됩니다. Helm `authSession.enabled`이면 `k8s-auth-session` meta
(BFF 세션 로그인, ADR 0011), `managerAuth.enabled`이면 `k8s-auth-manager-origin`/`-login`
meta(Dhub2.0 인증 위임 — UI가 Portal과 같은 OIDC cookie refresh를 사용하고 로컬 로그인
JWT cookie를 fallback으로 지원해 access token을 받아 Bearer로 사용)가 삽입됩니다. 기본/direct 렌더에는 meta와 인증
bootstrap 요청이 없고 rollback 시 nginx 설정과 함께 즉시 제거됩니다. 두 모드는 상호
배타이며 SSE(`sse.ts`)도 같은 토큰/세션 갱신 경로(`refreshAuth`)를 공유합니다.

## Dashboard Builder (#24)

`/dashboard-builder`는 embedded 표준 dashboard와 분리된 사용자 draft 목록입니다. 서버 capability가 enabled이고 `dashboard.editor`일 때만 자기 draft 편집·clone·submit controls를 노출하며 `platform.admin`은 제출본 approve controls만 봅니다. drag 중에는 네트워크를 호출하지 않고 pointer 종료 시 한 번 저장합니다. 키보드 이동/가로·세로 resize controls도 같은 deterministic 12x96 overlap 검사를 사용합니다. 409 충돌은 로컬 편집을 유지하고 최신본 reload 또는 로컬본 fork를 명시적으로 선택합니다. preview는 기존 aggregate `/overview` 한 건을 재사용합니다.

React + TypeScript 기반 Custom Observability UI입니다.
**MVP UI 화면이 모두 구현되어 있습니다** — Cluster Overview(#14), Nodes,
Namespace / Workload / Pod Drill-down(#15), Logs Explorer와 상관분석(#16),
Pod Topology, Alerts(#17).

개발 서버는 기본적으로 **MSW mock을 사용**합니다. 기존 53개 회귀 E2E는 명시적인
`--mode e2e` 번들을 사용하고, `make test-web-integration`은 기본 production
mock-off 번들과 실제 Go BFF fixture를 함께 검증합니다. 이 fixture 검증은 production
인증 전달과 실데이터 연결이 완료되었다는 의미는 아닙니다.

---

## 실행

```bash
# 저장소 루트에서
make install
make dev            # http://localhost:5173

# 또는
npm run dev --workspace @k8s-dashboard/web
```

개발 중 mock은 `VITE_USE_MOCK=false`로 명시해 끌 수 있습니다. Production은 기본
mock-off이며 명시적인 `VITE_USE_MOCK=true`에서만 mock을 켭니다. 두 경로 모두
같은 계약(`@k8s-dashboard/contracts`)을 사용합니다.

## 상태 시나리오

Cluster Overview는 쿼리 파라미터로 상태를 재현할 수 있습니다. 리뷰와 스크린샷 검증에 씁니다.

| URL | 재현되는 상태 |
|---|---|
| `/` | 정상 |
| `/?scenario=degraded` | Quickwit·Alertmanager 장애 + GreptimeDB 부분 응답 (Network 패널 누락) |
| `/?scenario=forbidden` | Event·Alert·Topology 섹션이 권한 부족으로 거절 |
| `/?scenario=empty` | 모든 지표 정상, 이상 엔티티 0건 |
| `/?cluster=prod-frankfurt` | 화면 전체 403 (접근 불가 클러스터) |
| `/namespaces/media?cluster=prod-tokyo` | 권한 없는 Namespace 직접 접근 → 403 (데이터 미노출) |
| `/logs?range=1d` | 로그 결과가 서버 상한(5,000줄)에 걸려 잘림 안내 표시 |

Scope·시간 범위·자동 갱신 주기는 모두 URL 쿼리(`cluster`, `ns`, `range`, `refresh`)에 있습니다.
장애 대응 중 링크 하나로 같은 화면을 공유할 수 있어야 하기 때문입니다.

## 구조

```text
src/
├── api/            # 유일한 데이터 진입점 (client · TanStack Query 훅)
├── app/            # AppShell, 자리 표시자 라우트
├── components/     # 공통 조각 (상태, primitives, LineChart, 컨트롤)
├── features/
│   ├── overview/   # Cluster Overview 화면과 패널
│   ├── dashboards/ # Git embedded Dashboard generic view와 폐쇄형 Widget Registry
│   └── drill/      # Namespace 목록·상세, Workload 상세, Pod 상세
├── lib/            # 포맷터
├── mocks/          # MSW 핸들러와 고정 데이터
├── state/          # URL 기반 대시보드 파라미터
└── styles/         # design-system 참조 + 앱 셸 레이아웃
```

## 화면

| 경로 | 화면 | 이슈 |
|---|---|---|
| `/` | Cluster Overview | #14 |
| `/nodes` | Nodes (노드 목록 · 용량 대비 요청량 · 노드별 Pod, 클러스터 범위 권한 필요) | — |
| `/namespaces` | Namespace 목록 | #15 |
| `/namespaces/:namespace` | Namespace 상세 (Workload 표 · 추세 · 이벤트) | #15 |
| `/workloads/:kind/:name?ns=` | Workload 상세 (replica · rollout · OwnerReference · Pod) | #15 |
| `/pods/:name?ns=&uid=` | Pod 상세 (Container · Owner 체인 · 로그 연결) | #15 |
| `/logs?ns=&uid=&levels=&q=&from=&to=` | Logs Explorer (히스토그램 · 구간 선택 · 커서 페이징) | #16 |
| `/topology?edge=` | Pod Topology (방향별 선 · 엣지 상세 · 시계열) | #16 |
| `/alerts?tab=&alert=` | Alerts (Active/Resolved · 상세 · deep link) | #17 |
| `/dashboards/:id` | `packages/dashboard-schema/dashboards/*.json` 자동 발견 Dashboard | #18 |
| `/resources?res=&q=&labels=&order=&item=` | Resources 진입 화면 — Explorer / Deployments / Secrets 탭 | ADR 0018 |
| `/deployments`, `/secrets` | Deployment · Secret 관리 (기존 화면 그대로. Resources 탭에서도 진입) | ADR 0014 |

### Resources (ADR 0018)

`/resources`는 리소스 작업의 진입점이고 탭이 세 개입니다. **Explorer**는 이 화면 안에서
렌더링하는 조회 전용 탐색기이고, **Deployments·Secrets** 탭은 기존 `/deployments`·`/secrets`
화면으로 이동합니다. 기존 라우트·화면·좌측 nav 항목은 그대로 두고 진입점만 추가한 것이라
기존 링크와 북마크는 계속 같은 화면을 엽니다. 탭은 WAI-ARIA tabs 패턴(좌우 화살표·Home/End,
`aria-selected`, `aria-controls`)을 따르고 선택 상태를 색 단독으로 전달하지 않습니다.

- 좌측 nav의 **Resources 항목과 `/resources` 화면은 서버가 준 `canExploreResources`로만**
  열립니다. 기존 관리 그룹의 `canManageWorkloads` 조건은 그대로입니다.
- Explorer는 BFF의 **catalog / list / detail 세 경로만** 부릅니다. Kubernetes를 직접
  호출하지 않고, `queryRef`처럼 서버에 등록된 대상만 조회합니다.
- **폴링하지 않습니다.** 자동 갱신 주기를 걸지 않고 "다시 조회"·"더 보기"처럼 사용자가
  일으킨 조작에서만 요청이 나갑니다. 필터도 타이핑마다가 아니라 "조회"에서 반영됩니다.
- **cursor 페이징입니다.** 서버가 준 불투명 cursor로만 이어보고 offset을 만들지 않습니다.
  cursor가 없으면 "마지막 페이지"라고 명시합니다.
- **일곱 상태를 구분합니다** — `ready`(목록) · `empty`(0건) · `syncing` · `unsupported`(406) ·
  `forbidden`(서버 RBAC) · `missing`(미제공) · `unavailable`(central·비활성). 같은 빈 화면으로
  접지 않습니다. 400·409·429처럼 상태가 아닌 오류는 상태로 위장하지 않고 이유를 그대로 알립니다.
- 상세는 **읽기 전용 YAML 모달**입니다. 편집·저장·복사 경로가 없고, Secret의
  `data`/`stringData`는 애초에 응답에 없습니다. 무엇이 제거됐는지는 감추지 않고
  redaction 안내에 함께 적습니다. Escape·닫기 버튼으로 닫히고 포커스는 모달 안에 갇힙니다.
- 생성·수정·삭제 컨트롤이 없습니다. 화면 전체가 조회 전용입니다.

## 설계 규칙

### 요청

- **화면 하나 = 요청 하나.** Cluster Overview 전체가 `GET /api/v1/clusters/{id}/overview` 한 번입니다.
  위젯마다 훅을 만들면 초기 로딩에서 N+1이 발생합니다. (이슈 #14 완료 기준)
  실측: 첫 진입 시 API 요청 2건(`/scope` + `/overview`).
- 브라우저는 GreptimeDB·Quickwit·Kubernetes API를 **직접 호출하지 않습니다.** `src/api/client.ts`가 유일한 통로입니다.
- `signal`을 그대로 넘겨 요청 취소가 네트워크까지 전파되게 합니다.
- 프런트엔드는 Raw Query를 만들지 않습니다. 서버의 `queryRef`를 씁니다.

### 상태

- **"데이터 없음 · 권한 없음 · upstream 장애"는 서로 다른 화면입니다.** 세 경우를 같은 빈 화면으로 처리하면
  운영자가 원인을 오판합니다. `Section<T>` 봉투와 `SectionView`가 이를 강제합니다.
- 부분 장애는 패널 단위입니다. 한 데이터소스가 죽어도 나머지 패널은 값을 유지하고,
  장애 패널은 테두리 + stale 배지로 표시합니다.
- 권한 부족은 재시도하지 않습니다.

### 갱신

- 자동 갱신은 **데이터만** 바꿉니다. `placeholderData: keepPreviousData` 덕분에 범위를 바꿔도
  화면이 비워지지 않고 값만 교체됩니다. (DOM 노드 유지 확인 완료)
- Step은 사용자가 고르지 않습니다. 범위에 따라 서버가 강제하고 UI는 현재 Step을 표시만 합니다.

### 드릴다운 (#15)

- **권한은 서버가 강제합니다.** 접근 불가한 Namespace를 URL로 직접 찍어도 403이 나가고
  화면은 권한 안내만 표시합니다. 목록·표가 부분적으로도 노출되지 않습니다.
- **Pod의 신원은 UID입니다.** 이름이 같아도 재생성된 Pod는 다른 인스턴스입니다.
  URL의 `uid`, 캐시 키, 표의 `key`가 모두 UID를 따릅니다.
- **큰 목록은 가상 스크롤입니다.** Workload 240개인 Namespace에서 실제 렌더 행은 30개 이하입니다.
- **갱신이 사용자 상태를 지우지 않습니다.** 필터 선택은 URL에, 스크롤 위치는 DOM에 남습니다.
  자동 갱신·범위 변경 후에도 유지되는 것을 테스트로 확인합니다.
- **OwnerReference 체인을 그대로 보여줍니다.** 롤아웃 중이면 ReplicaSet 두 세대가 함께 표시되고
  현재 세대가 표시됩니다.
- Pod 상세에서 Logs Explorer로 이동할 때 **같은 시간 범위와 Pod UID**를 그대로 넘깁니다.

### 로그 (#16, ADR 0003)

- **커서 페이징.** offset을 쓰지 않습니다. 로그가 계속 들어오는 동안 offset은 경계가 밀려
  중복·누락을 만듭니다. 커서는 (timestamp, id) 복합키이며 클라이언트는 해석하지 않습니다.
- **id는 충돌 불가능해야 합니다.** 같은 밀리초에 여러 줄이 들어오므로 timestamp만으로는 부족합니다.
- **마스킹은 서버에서만 합니다.** 응답의 `message`는 이미 가려진 문자열이고, UI는 `masked` 스팬으로
  "무엇이 가려졌는지"를 표시만 합니다. 원문 조회·복사 경로를 만들지 않습니다.
- **결과 상한은 서버가 강제합니다.** 상한에 걸리면 화면에 "잘렸다"고 명시합니다. 조용히 자르지 않습니다.
- **히스토그램·facet은 레벨 필터 이전 집합** 기준입니다. 그래야 꺼져 있는 레벨의 건수를 알 수 있습니다.
- 차트를 드래그하면 그 구간이 URL(`from`/`to`)에 들어가고 조회가 좁혀집니다.
  Kubernetes Event는 같은 축에 점선으로 겹칩니다.
- 펼침은 한 번에 한 줄만 허용합니다. 가변 높이가 여럿이면 가상 스크롤 오프셋이 추측이 되어
  스크롤이 튑니다.

### 토폴로지

- `A→B`와 `B→A`는 **별도의 선**입니다. 노드 중심선에서 각각 10px씩 반대로 밀어 평행하게 그립니다.
- 색은 상태, 두께는 트래픽 양, 프로토콜은 캡슐 텍스트입니다. 색으로 프로토콜을 구분하지 않습니다.
- 캡슐(라벨)은 **노드 박스와 다른 캡슐을 모두 피해** 배치합니다. 빈 자리가 없으면 겹침 면적이
  가장 작은 자리를 고릅니다. 첫 후보로 되돌아가면 라벨이 노드 이름을 가립니다.
- 레이아웃(column/row)은 서버가 정합니다. 클라이언트가 매번 배치를 계산하면 갱신할 때마다
  노드가 튀어 "어제 본 그림"과 달라집니다.
- 엣지 시계열은 **선택했을 때만** 조회합니다. 화면 단위 집계(ADR 0002)의 의도적 예외이며,
  사용자 조작으로 발생하는 추가 조회입니다.

### 알림 (#17)

- **조회 전용입니다.** 자체 평가 엔진을 만들지 않고 Grafana Alerting / Alertmanager의 상태를
  공통 모델로 정규화해 보여주기만 합니다. Rule 편집 · Silence · Routing 변경은 MVP 비목표이며
  화면에도 제공하지 않고 원본 시스템으로 보냅니다.
- **Alert backend 장애는 그 섹션만 죽입니다.** `Section`이 degraded로 내려와도 화면과 다른
  패널은 계속 동작합니다.
- **grouping 기준을 화면에 노출합니다.** "왜 12건이 1건으로 보이는가"를 설명할 수 없으면
  운영자는 화면을 믿지 않습니다.
- label의 namespace/workload를 Unified Entity Model로 매핑해 상세·로그 deep link를 겁니다.
  매핑에 실패하면 "매핑 없음"을 표시하고 원본 시스템으로 안내합니다.
- Alertmanager API v2는 현재 firing/suppressed만 제공합니다. history adapter가 없는 운영에서는
  Active는 정상 동작하고 Resolved와 resolved 포함 counts만 `history_not_configured`로 degraded됩니다.

### Mock 규칙

- **Pod 신원의 단일 원천은 `mocks/drilldown.ts`의 `primaryPod()`입니다.**
  Topology·로그·알림 mock이 각자 Pod 이름과 UID를 지어내면 화면 간 deep link가 404가 됩니다.
  mock끼리도 계약을 지켜야 화면 연결을 실제로 검증할 수 있습니다.

### 스타일

- 원시 hex·임의 픽셀값을 쓰지 않습니다. `design-system/tokens`의 역할 토큰만 참조합니다.
- 디자인 시스템 CSS는 복사하지 않고 `@import`로 그대로 참조합니다. 원천은 `design-system/`입니다. (ADR 0001)
- 차트는 라이브러리를 확정하기 전이라 얇은 자체 구현(`components/LineChart.tsx`)을 씁니다.
  ECharts/uPlot이 정해지면 이 파일만 교체합니다.

### 접근성

- 모든 폼 컨트롤에 라벨이 있고, 차트에는 `aria-label` 설명이 붙습니다.
- 상태는 색 단독으로 전달하지 않습니다. 배지의 글리프 + 텍스트가 항상 함께 갑니다.
- `main`/`nav` 랜드마크와 본문 건너뛰기 링크가 있습니다.
- `prefers-reduced-motion`을 존중합니다. 실시간 갱신 화면이라 특히 중요합니다.

## 남은 것

- 실제 API 연결 (`#8` Informer, `#9` Query Catalog, `#10` OIDC/RBAC 이후)
- Node 상세 화면 — 현재 Overview의 Node 항목은 Namespace 목록으로 되돌립니다
- 가상 스크롤을 라이브러리(TanStack Virtual)로 교체할지 결정 — 지금은 최소 구현
- 서버 측 pagination — 현재는 전체를 받아 클라이언트에서 가상화합니다

## 품질 게이트

- `make web-unit`: Vitest + Testing Library/jsdom 대표 unit/component 테스트
- `make contract-test`: 공통 OpenAPI/JSON Schema/TypeScript parity와 dashboard schema 테스트
- `make test-web`: 기존 전체 Playwright E2E. 초기 대시보드 요청은 `/scope` + `/overview` 2건 이하이며 취소 전파도 검증
- `make build-web-production && make web-performance`: 기본 mock-off production asset의
  raw/gzip 결정적 합계 예산(실측 baseline의 약 15% headroom). wall-clock은 gate로 쓰지 않음
- `make dependency-audit`: Vite 7.3.6의 high advisory를 제거하고, 브라우저에서 사용하지 않는
  React Router 기능의 moderate advisory 3건만 package/version/range/fix 기준으로 기한부 허용
