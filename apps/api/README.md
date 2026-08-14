# Observability API / BFF

화면 단위 집계 응답을 만드는 Go 서버입니다. Kubernetes 상태는 **watch 기반 informer 캐시**에서,
메트릭·로그·알림은 각 데이터소스 어댑터에서 가져옵니다.

언어 선택 근거는 [ADR 0004](../../docs/adr/0004-backend-language-go.md)에 있습니다.
한 줄로 줄이면 이렇습니다 — **클러스터에 주는 부하는 언어가 아니라 클라이언트 아키텍처가 결정합니다.**

## 지켜야 하는 것

이 세 줄이 깨지면 나머지 코드가 아무리 좋아도 의미가 없습니다.

1. **요청 처리 경로에서 Kubernetes API를 호출하지 않습니다.** 항상 lister(informer 캐시)에서 읽습니다.
   요청당 API 서버 호출은 **0회**입니다. `TestQueriesNeverCallTheAPIServer`가 이걸 셉니다.
2. **폴링하지 않습니다.** 자동 갱신은 브라우저 → 우리 백엔드까지이고, 백엔드 → API 서버는 watch 하나입니다.
   사용자가 100명이어도 클러스터 부하는 같습니다.
3. **Scope는 서버가 강제합니다.** 요청의 `cluster`/`ns`는 힌트일 뿐입니다.
   범위 밖이면 **부분 데이터도 만들지 않고** 403으로 끝냅니다.

## 실행

```bash
make dev-api                       # kubeconfig 또는 in-cluster 설정을 자동으로 찾습니다
KUBECONFIG=~/.kube/config make dev-api
```

기동 순서는 **informer 시작 → 최초 동기화 완료 대기 → HTTP 리스너 오픈**입니다.
동기화 전에 트래픽을 받으면 "Pod 0개"처럼 틀린 값이 정상처럼 보입니다.
`/readyz`는 캐시가 채워진 뒤에만 200을 돌려줍니다.

### 설정

| 환경변수 | 기본값 | 설명 |
|---|---|---|
| `ADDR` | `:8080` | 리스너 주소 |
| `KUBECONFIG` | (비움) | 비우면 in-cluster 설정 |
| `CLUSTER_ID` / `CLUSTER_NAME` | `default` | 응답에 실리는 클러스터 식별자 |
| `SCOPE_NAMESPACES` | `*` | 노출할 namespace. `*`면 전체 |
| `K8S_RESYNC` | `10m` | informer resync. **짧게 두지 마세요** — API 서버를 다시 치지는 않지만 CPU를 씁니다 |
| `K8S_EVENT_FIELD_SELECTOR` | `type=Warning` | Event는 대부분의 클러스터에서 가장 수가 많습니다. 전부 watch하면 대시보드가 부하의 주범이 됩니다 |
| `K8S_QPS` / `K8S_BURST` | `20` / `30` | client-side rate limit |
| `K8S_DISABLE_PROTOBUF` | `false` | protobuf를 지원하지 않는 aggregated API 뒤에서만 켭니다 |
| `CACHE_TTL` | `5s` | 화면 응답 재사용 시간. 0으로 두면 자동 갱신 사용자 수만큼 팬아웃이 늘어납니다 |
| `USE_DEMO_DATA` | `true` | GreptimeDB/Quickwit/Alertmanager 없이 결정적 값으로 실행. **실주소가 설정된 데이터소스는 이 값과 무관하게 실제 어댑터를 씁니다** |
| `ALLOWED_ORIGIN` | (비움) | 개발 중 Vite 오리진 허용 |

### 실데이터소스 설정

| 환경변수 | 기본값 | 설명 |
|---|---|---|
| `GREPTIME_URL` | (비움) | GreptimeDB HTTP 주소. 예: `http://greptimedb:4000`. 비우면 메트릭은 데모 또는 degraded |
| `GREPTIME_DB` | `public` | 데이터베이스 이름 |
| `GREPTIME_USERNAME` / `GREPTIME_PASSWORD` | (비움) | Basic 인증. **브라우저로 나가는 응답 어디에도 실리지 않습니다** |
| `GREPTIME_TIMEOUT` | `10s` | 질의 1건 상한 |
| `GREPTIME_MAX_POINTS` | `1000` | **전역** 포인트 상한. 카탈로그의 쿼리별 `maxDataPoints`보다 작으면 이 값이 이깁니다 |
| `QUERY_CATALOG_DIR` | (비움) | 쿼리 카탈로그 디렉터리. 비우면 임베디드 기본 카탈로그. 지정하면 기본을 **대체**합니다(병합 없음) |
| `QUICKWIT_URL` | (비움) | Quickwit 주소. 예: `http://quickwit:7280` |
| `QUICKWIT_INDEX` | `k8s-logs` | 로그 인덱스 id |
| `QUICKWIT_USERNAME` / `QUICKWIT_PASSWORD` | (비움) | Basic 인증 |
| `QUICKWIT_TIMEOUT` | `10s` | 검색 1건 상한 |
| `QUICKWIT_MAX_PAGE` | `500` | 페이지 크기 상한. 브라우저가 큰 값을 보내도 넘지 못합니다 |
| `QUICKWIT_MAX_LINES` | `5000` | 조회 범위 총량 상한. 넘으면 `truncated`로 알립니다 |
| `QUICKWIT_FIELDS` | (비움) | 인덱스 필드 이름 재정의. `message=body.message,level=severity_text` 형식. 키: `timestamp` `level` `message` `namespace` `pod_name` `pod_uid` `container` `workload_kind` `workload_name` `node` `trace_id` `span_id` |

Quickwit의 `namespace` `pod_name` `pod_uid` `level` `container` `workload_name` 필드는
필터·집계에 쓰이므로 인덱스에서 **fast field**(raw 토크나이저)여야 합니다.

### 실어댑터가 지키는 규칙

- **질의는 등록형 쿼리 카탈로그에서만 나옵니다.** 프런트는 패널 id·검색어만 보낼 수
  있고, PromQL 템플릿·escape·Step 계산은 `internal/querycatalog`(#9)가,
  ES 필터 조립은 `quickwit/query.go`가 맡습니다. 등록되지 않은 queryRef는 실행
  경로가 없고, 사용자 검색어는 match 노드의 **값**으로만 들어가므로 연산자
  주입으로 Scope 필터를 우회할 수 없습니다. 카탈로그 규칙은 아래 §쿼리 카탈로그 참고.
- **Pod 신원은 카탈로그에서 빌려옵니다.** 메트릭 라벨은 pod 이름이지만 화면 신원은
  UID입니다. UID → 이름 변환과 facet의 UID·Kind는 informer 캐시(`CatalogPods`)를
  거칩니다. 인덱스의 uid를 믿으면 사라진 Pod의 딥링크가 404가 됩니다.
- **로그 커서는 (경계 timestamp, 경계 id 집합)입니다.** offset(`from`)은 어떤 요청에도
  없습니다. 같은 밀리초에 몰린 로그가 경계에 걸려도 중복·누락이 없습니다. (ADR 0003)
- **오류는 두 가지로만 분류합니다.** 연결 실패·타임아웃·5xx는 `datasource.ErrUnavailable`,
  4xx는 `upstream.ErrBadQuery`. 에러 문자열에 내부 주소·질의를 담지 않습니다 —
  섹션 degraded 사유로 노출될 수 있기 때문입니다. 일시 오류는 **1회만** 재시도합니다.

## 구조

```text
cmd/api/                 기동 순서와 신호 처리
internal/
  clusterstate/          watch 기반 informer 캐시 (이 서비스의 심장)
    client.go              protobuf 콘텐츠 협상, metadata 클라이언트
    store.go               informer 구성 · 인덱스 · 동기화
    normalize.go           Pod/Workload/Node 상태를 화면 어휘로 정규화
    query.go               lister 위의 조회. API 서버를 호출하지 않습니다
    catalog.go             데이터소스가 빌려 쓰는 Pod 신원
  datasource/            메트릭·로그·알림·토폴로지 어댑터 경계
    mask/                  로그 마스킹 (서버에서만)
    demo/                  결정적 데모 어댑터
    upstream/              공용 HTTP 클라이언트 — 오류 분류 · 1회 재시도 · 취소 전파
    greptime/              GreptimeDB 메트릭 어댑터 (#6) — Prometheus 호환 API
      queries.go             서버 측 쿼리 카탈로그. 프런트는 패널 id만 고를 수 있습니다
    quickwit/              Quickwit 로그 어댑터 (#7) — ES 호환 검색 · 커서 페이징
  querycatalog/          등록형 쿼리 카탈로그 (#9) — 실행 가능한 질의의 유일한 원천
    defaults/              Git에 커밋되고 바이너리에 임베드되는 기본 카탈로그(YAML)
  httpapi/               화면 단위 엔드포인트 · Scope 강제 · 섹션 봉투
  contract/              packages/contracts와 같은 JSON을 만드는 Go 타입
  timerange/             범위별 강제 Step, Custom 30일 상한
  scope/                 서버가 강제하는 조회 범위
  cache/                 TTL + singleflight
  testcluster/           테스트 픽스처 (프로덕션 코드는 임포트하지 않습니다)
```

### informer 구성

| 리소스 | informer | 이유 |
|---|---|---|
| Pod, Node, Deployment, StatefulSet, DaemonSet, CronJob | typed (protobuf 협상) | spec/status가 화면에 필요합니다 |
| ReplicaSet | **metadata-only** (`PartialObjectMetadata`) | 소유 관계와 revision 애노테이션만 쓰므로 spec/status를 받지 않습니다 |
| Event | typed + `type=Warning` 필드 셀렉터 | 수가 가장 많은 리소스입니다. 범위를 좁혀서 watch합니다 |

인덱스는 세 개입니다 — `podByOwner`, `replicaSetByOwner`, `eventByInvolved`.
Deployment → Pod 조회가 전체 순회 대신 인덱스 두 번으로 끝납니다.

## 엔드포인트

화면 하나당 하나입니다. 위젯마다 만들지 않습니다. ([ADR 0002](../../docs/adr/0002-screen-scoped-aggregated-endpoints.md))

```text
GET /api/v1/scope
GET /api/v1/clusters/{clusterId}/overview
GET /api/v1/clusters/{clusterId}/namespaces
GET /api/v1/clusters/{clusterId}/namespaces/{namespace}
GET /api/v1/clusters/{clusterId}/workloads/{kind}/{name}?ns=
GET /api/v1/clusters/{clusterId}/pods/{name}?ns=&uid=
GET /api/v1/clusters/{clusterId}/logs?ns=&levels=&q=&cursor=
GET /api/v1/clusters/{clusterId}/topology?ns=
GET /api/v1/clusters/{clusterId}/topology/edges/{edgeId}/series
GET /api/v1/clusters/{clusterId}/alerts?ns=
GET /healthz   GET /readyz
```

### 응답 규칙

- 패널마다 `Section<T>`로 감쌉니다. `ok | empty | forbidden | degraded` — **세 가지 실패가 서로 다릅니다.**
  "결과 0건"과 "권한 없음"과 "데이터소스 장애"를 하나로 접으면 사용자가 대응을 못 합니다.
- 화면 전체가 실패할 때만 최상위 에러(`{code, message}`)를 씁니다. 403 / 400이 여기에 해당합니다.
- degraded 사유에 내부 주소·질의·스택트레이스를 담지 않습니다.

## 테스트

```bash
make api-test     # 단위 테스트 — fake clientset. 클러스터 불필요
make api-itest    # 통합 테스트 — 실제 kube-apiserver 대상
```

값이 맞는지보다 **규칙이 지켜지는지**를 봅니다.

| 테스트 | 지키는 규칙 |
|---|---|
| `TestQueriesNeverCallTheAPIServer` | 요청당 API 서버 호출 0회 (ADR 0004) |
| `TestInitialSyncUsesWatchNotPolling` | 리소스당 LIST 1회 + WATCH 1회 |
| `TestNamespaceOutsideScopeLeaksNothing` | URL을 고쳐도 범위 밖 데이터가 나가지 않음 |
| `TestCacheKeyIncludesScope` | 권한이 다른 사용자가 캐시를 공유하지 않음 |
| `TestLogCursorPagingHasNoDuplicatesOrGaps` | 커서 페이징 정확성 (ADR 0003) |
| `TestLogMessagesAreMaskedBeforeLeavingTheServer` | 원문이 서버 밖으로 나가지 않음 |
| `TestDatasourceOutageDegradesSectionsNotThePage` | 부분 장애가 화면 전체를 죽이지 않음 |
| `TestOverviewIsOneRequestWithoutPerWidgetFanout` | 요청 하나 안에서도 데이터소스 팬아웃 없음 |
| `TestDatasourceTargetCarriesAllowedNamespaces` | 허용 namespace 목록이 어댑터 Target까지 전달됨 |
| `TestCursorPagingHasNoDuplicatesOrGaps` (quickwit) | 실어댑터 커서에 중복·누락 없음, 경계 충돌 포함 (ADR 0003) |
| `TestOffsetPagingIsNeverUsed` (quickwit) | 요청 본문에 offset(`from`)이 존재하지 않음 (ADR 0003) |
| `TestScopeFilterIsAlwaysInjected` (quickwit) | 검색어로 namespace 필터를 우회할 수 없음 |
| `TestMessagesAreMaskedBeforeLeavingTheServer` (quickwit) | 원문이 서버 밖으로 나가지 않음 |
| `TestRangeQueryCarriesWindowAndScope` (greptime) | 범위·Step·Scope 매처가 서버 확정 값 그대로 |
| `TestStepWidensToRespectMaxDataPoints` (greptime) | 장기 조회에서도 포인트 상한 준수 |
| `TestUpstreamFailureIsClassifiedAndRetriedOnce` (양쪽) | 표준 오류 분류 · 재시도는 1회 |
| `TestDefaultCatalogIsValid` (querycatalog) | Git의 기본 카탈로그가 항상 유효 — CI 검출 지점 (#9) |
| `TestRangeQueryWithoutScopeIsRejected` (querycatalog) | $__scope 없는 range 질의는 로드 거부 |
| `TestVariableValuesAreConstrainedAndQuoted` (querycatalog) | 변수는 allowlist + 라벨 값 리터럴 — matcher 조각 삽입 불가 |
| `TestUnregisteredPanelIsNotExecuted` (greptime) | 미등록 패널 id는 질의가 나가지 않음 (#9) |

### 실클러스터 통합 테스트

fake clientset은 **우리 코드가 규칙을 지키는지**는 보여주지만, API 서버가 실제로 protobuf를
협상하는지 · 필드 셀렉터를 서버에서 걸러주는지 · watch가 몇 ms 만에 변경을 전달하는지는
보여주지 못합니다. 그건 진짜 서버에서만 나옵니다.

```bash
# 1) 로컬에서 kube-apiserver를 띄워서 (클러스터 불필요)
KUBEBUILDER_ASSETS=/path/to/envtest/bin make api-itest

# 2) 실제 클러스터에 겨눠서 — 기본 동작은 읽기 전용입니다
ITEST_KUBECONFIG=~/.kube/config make api-itest
```

| 환경변수 | 효과 |
|---|---|
| `ITEST_KUBECONFIG` | 이 kubeconfig의 클러스터를 대상으로 합니다. 없으면 `KUBECONFIG`를 봅니다 |
| `KUBEBUILDER_ASSETS` | 위가 없을 때, 이 디렉터리의 `etcd`·`kube-apiserver`를 직접 띄웁니다 |
| `ITEST_MUTATE=1` | 상태 반영 지연을 측정합니다. **임시 namespace와 Pod를 만듭니다** |
| `ITEST_SERVICE_ACCOUNT=ns:name` | 배포된 ServiceAccount의 실제 권한을 SubjectAccessReview로 검사합니다 |

둘 다 없으면 테스트는 실패가 아니라 **skip**입니다. 클러스터가 없다고 CI가 빨개지면 안 됩니다.

검증하는 것:

| 테스트 | 확인 |
|---|---|
| `TestLiveProtobufIsActuallyNegotiated` | 응답이 실제로 protobuf로 오는지. ADR 0004에서 Go를 고른 1순위 근거입니다 |
| `TestLiveInitialSyncIsOneListPlusOneWatchPerResource` | 리소스당 LIST 1회 + WATCH 1회. 폴링이 섞이지 않았는지 |
| `TestLiveServingCausesZeroAPICalls` | 화면을 21회 그리는 동안 API 서버 호출이 **0회**인지 |
| `TestLiveEventWatchIsNarrowedServerSide` | Event 요청 URL에 `fieldSelector=type=Warning`이 실제로 붙는지 |
| `TestLiveEveryPodNormalizesToAKnownState` | 실클러스터의 온갖 상태 조합이 전부 알려진 severity로 떨어지는지 |
| `TestLiveOwnerChainMatchesRealCluster` | Deployment마다 현재 세대가 정확히 1개인지 |
| `TestLivePodStateChangeReachesCacheQuickly` | 변경 → 캐시 반영이 5초 안인지 (`ITEST_MUTATE=1`) |
| `TestLiveDeployedServiceAccountCannotReadSecrets` | 필요한 권한은 되고 Secret·exec·delete는 거절되는지 |
| `TestLiveWatchRecoversFromCompactedHistory` | watch 단절 + etcd compaction 이후 캐시가 회복되는지, 그 비용이 리소스당 LIST 1회인지 |

측정 예 (kube-apiserver v1.31, Pod 3개):

```
응답 Content-Type — protobuf 16 · json 0 · 기타 0
리소스 8종 · LIST 8회 · WATCH 8회
화면 21회 · API 서버 추가 호출 0회
Event 요청: /api/v1/events?fieldSelector=type%3DWarning&limit=500&resourceVersion=0
생성 → 캐시 반영 8ms · 변경 → 캐시 반영 25ms
단절·compaction 이후 변경 → 캐시 반영 1.257s
410 Gone 0회 · 재LIST map[/api/v1/events:1 /api/v1/nodes:1 /api/v1/pods:1 ...]
```

#### etcd compaction에 대해 확인한 것

"resourceVersion 만료(410 Gone)에서 회복하는가"를 확인하려다 두 가지를 실측했고,
둘 다 예상과 달라서 남겨둡니다.

1. **watch가 살아 있는 동안의 compaction은 아무 일도 일으키지 않습니다.**
   etcd는 이미 따라잡은(synced) watcher에게 compaction을 통지하지 않습니다.
   끊기는 것은 아직 과거를 따라가는 중인 watcher뿐입니다.
   그래서 테스트는 **watch를 먼저 끊고** compact합니다.
2. **재연결 경로에서 410은 나오지 않는 것이 정상입니다.**
   reflector는 재연결 시 `resourceVersion=<마지막 값>`으로 LIST하는데,
   LIST의 resourceVersion은 "그 값 **이상으로** 최신"이라는 뜻입니다.
   API 서버는 현재 revision으로 quorum read를 하고, compaction된 과거를 읽지 않습니다.
   만료는 그 RV로 **watch**를 걸 때만 발생합니다.

그래서 이 테스트가 실제로 지키는 것은 "410 처리"가 아니라 **회복과 회복 비용**입니다 —
끊긴 리소스마다 재LIST가 정확히 1회이고, 그보다 많으면 실패합니다.
잘못된 재연결의 실제 위험은 낡은 값이 아니라 **LIST 폭풍**이기 때문입니다.

### 실데이터소스 통합 테스트

httptest 계약 테스트는 우리 쪽 규칙을 검증하지만, 실제 GreptimeDB가 PromQL을
받아주는지 · 실제 Quickwit의 정렬·집계가 기대와 같은지는 진짜 인스턴스에서만
나옵니다. `make api-itest`에 함께 들어 있습니다. env가 없으면 skip입니다.

```bash
# 예: 로컬 컨테이너를 띄우고
docker run -d --name greptime -p 4000:4000 greptime/greptimedb standalone start --http-addr 0.0.0.0:4000
docker run -d --name quickwit  -p 7280:7280 quickwit/quickwit run

# 읽기 전용 검증 (운영 인스턴스에 겨눠도 안전합니다)
GREPTIME_ITEST_URL=http://localhost:4000 \
QUICKWIT_ITEST_URL=http://localhost:7280 QUICKWIT_ITEST_INDEX=k8s-logs \
  make api-itest

# 쓰기 검증 — 전용 리소스만 만들었다 지웁니다
ITEST_MUTATE=1 GREPTIME_ITEST_URL=... QUICKWIT_ITEST_URL=... make api-itest
```

| 테스트 | 확인 |
|---|---|
| `TestLiveGreptimeRangeQueryRoundTrip` | 기본 카탈로그 range 질의 전부가 실서버에서 실행됨 (없는 namespace → 빈 시리즈) |
| `TestLiveGreptimeInstantQueryRoundTrip` | 사용량 instant 질의 왕복과 응답 파싱 |
| `TestLiveGreptimeScopeIsEnforcedOnRealData` | `ITEST_MUTATE=1` — 전용 테이블에 두 namespace를 넣고 Scope 매처가 한쪽만 돌려주는지 |
| `TestLiveQuickwitCursorAdvancesWithoutDuplicates` | 실데이터 위 커서 전진 · 페이지 간 중복 0 · 내림차순 정렬 |
| `TestLiveQuickwitEndToEndPaging` | `ITEST_MUTATE=1` — 전용 인덱스에 timestamp 충돌 문서를 넣고 전체 순회 중복·누락 0, Scope, 서버 마스킹, 레벨 필터 |

쓰기 검증이 만드는 것은 GreptimeDB `k8s_dashboard_itest_metric` 테이블과
Quickwit `k8s-dashboard-itest` 인덱스뿐이며 끝나면 지웁니다.
운영 메트릭 테이블·로그 인덱스에는 절대 쓰지 않습니다.

### 쿼리 카탈로그 (#9)

실행 가능한 질의의 유일한 원천은 `internal/querycatalog/defaults/*.yaml`입니다.
Git이 진실이고, 빌드 시 바이너리에 임베드되며, 시작 단계에서 검증됩니다.
검증 실패면 서버가 뜨지 않습니다 — 잘못된 카탈로그로 뜬 서버는 빈 화면을
정상처럼 보여주기 때문입니다.

```yaml
- ref: metrics.cpu.used          # queryRef — 화면은 이 이름만 압니다
  type: promql_range             # promql_range | promql_instant
  unit: cores
  expr: sum(rate(container_cpu_usage_seconds_total{$__scope,container!=""}[$__rate]))
  minStep: 60s                   # 이보다 좁은 Step 요청은 올립니다
  maxDataPoints: 1000            # 넘으면 Step을 넓힙니다
  timeout: 10s
  maxRange: 720h                 # 최대 조회 기간
```

규칙 — 어기면 로드가 실패합니다.

- range 질의 `expr`에는 `$__scope`가 반드시 있습니다. Scope를 잊은 질의는
  존재할 수 없습니다. 클러스터 전체 질의는 `clusterWide: true`를 명시합니다.
- 내장 자리표시자는 `$__scope`·`$__rate` 둘뿐입니다. 오타는 로드 오류입니다.
- 변수는 `variables`에 allowlist(values/pattern)로 선언해야 하고, 렌더링은
  **라벨 값 리터럴**로만 됩니다. matcher·표현식 조각을 끼울 자리가 없습니다.
- 패널 시리즈는 등록된 `promql_range` ref만 가리킬 수 있습니다. ref 중복 금지,
  오타 필드 금지(KnownFields).
- 로그 조회 한계(`logs.search.maxPageSize` 등)도 카탈로그가 선언하며
  환경변수보다 우선합니다.

같은 지표는 어느 화면(Overview·Namespace·Workload·Pod)에서든 같은 ref를
쓰므로 정의가 갈라질 수 없습니다. Raw query expert mode는 MVP 비목표이며
이 계층에 그 경로가 없습니다.

### RBAC

`deploy/rbac/k8s-dashboard-api.yaml`이 최소 권한입니다. 읽기 verb만 있고,
Secret·ConfigMap·`pods/exec`·`pods/log`가 없습니다. 적용한 뒤 실제로 그런지 확인합니다.

```bash
kubectl apply -f deploy/rbac/k8s-dashboard-api.yaml
ITEST_KUBECONFIG=~/.kube/config \
  ITEST_SERVICE_ACCOUNT=k8s-dashboard:k8s-dashboard-api make api-itest
```

## 아직 없는 것

- Alertmanager/Grafana Alerting 실제 클라이언트 (#17 잔여). 알림·토폴로지는
  데모 또는 degraded입니다. 메트릭·로그는 `GREPTIME_URL`·`QUICKWIT_URL`을 설정하면
  실제 어댑터로 동작합니다 (#6·#7 구현됨 — 실인스턴스 검증은
  `GREPTIME_ITEST_URL`/`QUICKWIT_ITEST_URL`로 `make api-itest`가 수행합니다).
- OIDC/SubjectAccessReview 기반 Scope (#10). `scope.Resolver` 인터페이스 뒤에 끼우면 핸들러는 바뀌지 않습니다.
- Redis 캐시 (#11). 지금은 프로세스 내 TTL + singleflight입니다.
- 통합 테스트의 CI 연결. 지금은 로컬에서만 돕니다 (#21).
- 멀티 클러스터. 지금은 프로세스당 클러스터 하나입니다.
