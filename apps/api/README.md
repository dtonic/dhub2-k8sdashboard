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
| `USE_DEMO_DATA` | `true` | GreptimeDB/Quickwit/Alertmanager 없이 결정적 값으로 실행 |
| `ALLOWED_ORIGIN` | (비움) | 개발 중 Vite 오리진 허용 |

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
make api-test
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

## 아직 없는 것

- GreptimeDB / Quickwit / Alertmanager 실제 클라이언트. 지금은 `USE_DEMO_DATA=true`의 결정적 어댑터를 씁니다.
  끄면 해당 섹션이 degraded로 내려가고 화면은 그대로 동작합니다.
- OIDC/SubjectAccessReview 기반 Scope. `scope.Resolver` 인터페이스 뒤에 끼우면 핸들러는 바뀌지 않습니다.
- 멀티 클러스터. 지금은 프로세스당 클러스터 하나입니다.
