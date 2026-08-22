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
├── app/            # AppShell, 라우트 레지스트리(routes.tsx), Command Palette, 최근 항목 저장
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

### Command Palette와 전역 검색 (ADR 0023)

정적 라우트의 단일 원천은 `src/app/routes.tsx`입니다. 라우터(`main.tsx`)·좌측 nav
(`AppShell`)·팔레트가 **같은 배열**을 읽으므로 셋이 어긋날 수 없습니다. catch-all(`*`)만
레지스트리 밖에 있습니다 — 라우팅이 아니라 리다이렉트이기 때문입니다. 경로 집합은 이
슬라이스에서 바뀌지 않았고 `/resources`·`/deployments`·`/secrets`는 그대로 서로 다른 화면입니다.

| 단축키 | 동작 |
|---|---|
| `Cmd/Ctrl + K` | 팔레트 열기·닫기. IME 조합 중이거나 다른 모달(`dialog`·`alertdialog`)이 열려 있으면 열지 않습니다 |
| `Tab` `Shift+Tab` | 다이얼로그 안에서만 돕니다 — 뒤 화면으로 빠져나가지 않습니다 |
| `↑` `↓` `Home` `End` | 결과 이동(끝에서 순환) |
| `Enter` | 선택 항목 열기 |
| `Esc` | 포커스가 어디에 있든 닫고 **열기 전 포커스로 정확히 한 번 복귀** |
| `/` (`/resources`에서만) | Explorer의 이름 검색 입력으로 포커스. 팔레트를 열지 않습니다 |

`/`는 입력·textarea·select·contenteditable 안, IME 조합 중, 모달이 열려 있을 때는
동작하지 않습니다. 하나라도 빠지면 타이핑이 사라집니다.

- 팔레트의 결과는 **이동**(레지스트리, capability 필터)과 **리소스 찾기** 두 그룹입니다.
  리소스 그룹은 서버가 준 `canExploreResources`로만 열리고, 이동 그룹은 기존 nav와 같은 규칙입니다.
- 리소스 결과의 출처는 BFF의 `/resources/search` **하나**입니다. Kubernetes를 직접 부르지 않고
  폴링하지 않습니다 — 입력이 멈춘 뒤 200ms에 한 번 나갑니다.
- **취소는 실제로 요청을 끊습니다.** 입력이 앞서면 디바운스를 기다리지 않고 그 질의 하나를
  `cancelQueries({ exact: true })`로 중단합니다. 최근 항목 요청은 닫을 때뿐 아니라 **입력을
  시작해 검색으로 넘어갈 때와 클러스터를 바꿀 때**도 끊깁니다 — 그때부터 그 응답은 화면에
  쓰이지 않습니다.
  `enabled: false`만으로는 부족합니다 — QueryObserver는 마운트된 채 남고 이미 나간 fetch는
  계속 달립니다. 취소는 키를 정확히 지목하므로 다른 화면의 질의는 건드리지 않습니다.
- **화면의 입력과 손에 든 결과가 어긋나면 결과를 쓰지 않습니다.** 디바운스가 따라오지 못한
  동안에는 이전 입력의 결과를 렌더하지도, degraded/truncated를 알리지도, Enter·클릭으로
  실행하지도 않고 "찾는 중"으로 답합니다. 0·1자로 지우면 즉시 idle·short이 됩니다.
- **질의 2..64자, 결과 최대 50건**으로 서버 상한과 같은 값을 UI도 강제합니다.
- **지어낸 상태를 만들지 않습니다.** 검색 인덱스에 status가 없으므로 결과에는 kind·이름·
  namespace와 어느 필드에 걸렸는지만 표시합니다.
- `syncing` · `unavailable` · `degraded`(사유 포함) · `empty` · `error` · 상한 절단을 서로 다른
  문구로 구분합니다. 같은 빈 화면으로 접지 않습니다.
- 결과를 고르면 기존 `/resources` deep link로 이동합니다 — `res`(GVR) · `ns` · `item`(ns/name/uid).

**최근 항목**은 `localStorage`에 버전이 붙은 봉투로 **브라우저 전체 20개**, UID와 이동 경로만
담습니다. 표시할 제목은 열 때마다 서버 `/resources/recent`가 다시 정합니다 — 권한이 사라졌거나
같은 이름의 다른 객체로 교체된 항목이 오래된 제목으로 남지 않게 하기 위해서입니다.

- **클러스터가 신원의 일부입니다.** 목록 하나에 여러 클러스터가 섞여 살지만 읽고 보내는 것은
  활성 클러스터의 참조뿐이고, 중복 판정도 `clusterId + GVR + UID`입니다. 클러스터를 신원에서
  빼면 다른 클러스터의 같은 UID가 같은 객체로 보이고, 클러스터를 바꾼 순간 옛 클러스터의
  참조가 새 클러스터 엔드포인트로 나갑니다. 장벽이 **열기 회차와 클러스터 둘 다**에 묶여
  있어 화면에서 클러스터를 바꾸면 그 요청을 끊고 새 클러스터의 목록을 다시 읽습니다.
- **서버로 가는 ref에는 클러스터가 들어가지 않습니다.** 엔드포인트 경로가 이미 소유합니다.
- 저장 항목은 서버 `ValidateGVRSegments`·`safeCursorSegment`와 **같은 규칙**으로 검증합니다
  (group은 `core` 또는 DNS1123 subdomain, version·resource는 DNS1035 label). 오염된 항목은
  인코딩 전에 빠집니다 — 하나가 400을 받으면 같은 배치의 정상 참조까지 함께 죽습니다.
- 최근 항목은 팔레트에서 고를 때뿐 아니라 **Resource Explorer 상세가 성공했을 때**도 남습니다.
  신원의 근거는 URL이 아니라 서버 응답이고, 실패·미확정 조회는 남기지 않습니다.

- **재확인이 끝나기 전에는 캐시된 제목을 쓰지 않습니다.** 도는 동안에는 목록 대신 "다시
  확인하는 중"입니다. 옛 값을 먼저 그려 두면 권한을 잃은 항목이 잠깐이라도 보입니다.
- **저장소를 다 읽은 뒤에만 조회를 켭니다.** 읽기 전 빈 목록으로 켜면 저장된 항목이 있는데도
  0건 probe가 헛나가고 곧 두 번째 질의가 따라붙습니다. 닫으면 이 장벽이 되돌아가므로 다시
  열 때마다 저장소를 다시 읽고 다시 물어봅니다.
- **참조가 0개여도 정확히 한 번 물어봅니다.** 검색 플래그는 `canExploreResources`를 바꾸지
  않으므로, 0건 probe의 응답만이 "최근이 비었다"와 "검색이 꺼졌다"를 구분해 줍니다. 실패는
  조용한 빈 목록이 아니라 사유(`search_unavailable`·`resources_unavailable`은 "사용할 수 없음",
  그 외는 오류)로 보여줍니다.
- 요청을 나누는 기준은 **request target 6KiB**입니다 — 프록시가 보는 `pathname + "?" + query`
  전체(클러스터 경로·모든 `ref=`와 `&`·apiGet이 덧붙이는 파라미터 포함)를 UTF-8 바이트로
  잽니다. 서버 상한은 8KiB입니다. 재는 방법은 **`apiGet`과 같은 `URL`·`URLSearchParams`로 실제
  후보를 만들어 보는 것**이고 `encodeURIComponent` 산술로 어림잡지 않습니다 — `URLSearchParams`는
  `~`를 `%7E`로 늘리지만 `encodeURIComponent`는 그대로 둡니다. 참조 0개인 probe까지 같은 자로
  먼저 재고, 그것조차 들어가지 않으면 **요청을 하나도 만들지 않고** 실패합니다.
- 덩어리는 **하나의 취소 신호를 공유**하므로 닫으면 남은 덩어리는 아예 나가지 않습니다.
  나눈 순서가 곧 원래 순서이므로 이어 붙이면 최신순이 보존됩니다.
- 저장소 정리는 **완전히 성공한 해석 뒤에만**, 그리고 **이번에 물어본 참조 중** 해석되지 않은
  것만 지웁니다. 중간에 끊긴 결과로 지우면 살아 있는 항목을 잃고, "응답에 없는 전부"를 지우면
  다른 클러스터의 참조나 요청 뒤에 다른 탭이 넣은 항목까지 사라집니다.
- 정리는 **저장소만** 바꿉니다. 이번 열기의 질의 키는 그대로 두므로 방금 받은 응답을 계속
  보여주고, 전부 사라진 경우에도 뒤따르는 0건 probe가 나가지 않습니다. 줄어든 목록은 다음에
  열 때 반영됩니다.

**롤백** — 검색만 끄려면 API에 `RESOURCE_EXPLORER_SEARCH_ENABLED=false`를 주면 됩니다
(Helm은 `api.config`에 넣습니다). 그때 팔레트는 이동 그룹만 남고 리소스 그룹은 이유를
명시한 안내로 바뀝니다. Explorer 전체를 끄는 스위치는 기존 `RESOURCE_EXPLORER_ENABLED`입니다.

**알려진 P2 비용** — 검색 색인 부트스트랩 때문에 API 기동 직후 CPU·메모리 정점이
카탈로그/목록만 쓸 때보다 높습니다. 첫 질의는 색인이 아직 준비되지 않았으면 `syncing`으로
답할 수 있습니다. 두 값 모두 상한이 서버에 있고 초과분은 `degraded`로 노출됩니다.

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
