# ADR 0004 — Observability API/BFF는 Go로 구현한다

- 상태: Accepted
- 날짜: 2026-08-13
- 결정자: @xenx96
- 관련: 이슈 [#8](https://github.com/xenx96/k8s-dashboard/issues/8) · README §6 기술 스택, §11 성능 원칙 · ADR 0002

## 배경

백엔드 언어를 Go와 Rust 중에서 고릅니다. 판단 기준은 **1순위가 조회 대상 Kubernetes 클러스터에
주는 부하이고, 2순위가 대시보드 백엔드 자체의 최적화**입니다. 개발 편의나 팀 선호는 뒤에 둡니다.

먼저 짚어야 할 것이 있습니다. **클러스터에 주는 부하는 언어가 아니라 클라이언트 아키텍처가 결정합니다.**

| 접근 | API 서버가 받는 부하 |
|---|---|
| 요청마다 `LIST /api/v1/pods` | 요청 수 × 전체 Pod 직렬화. 최악. 대시보드가 클러스터를 죽입니다. |
| 주기적 폴링 + 캐시 | 폴링 주기마다 전체 LIST. 사용자가 없어도 부하가 상수로 발생. |
| **watch 기반 shared informer** | 최초 LIST 1회 + 이후 변경분만 스트리밍. 사용자 수·요청 수와 무관. |

세 번째가 유일하게 맞는 답이고, 이건 Go로도 Rust로도 구현할 수 있습니다.
따라서 언어 선택은 "**informer를 얼마나 정확하고 저렴하게 구현할 수 있는가**"의 문제로 좁혀집니다.

## 결정

**Go를 선택한다.**

1순위 기준(클러스터 부하)에서 Go가 유리한 지점이 구체적으로 존재하고, 2순위 기준(백엔드 최적화)에서
Rust의 우위는 이 워크로드에서 실질적 차이를 만들지 않기 때문입니다.

### 1순위 — 클러스터 부하에서 Go가 유리한 지점

- **protobuf 콘텐츠 협상.** Kubernetes API 서버는 내장 타입에 대해 `application/vnd.kubernetes.protobuf`를
  지원하고, client-go는 이를 협상해 쓸 수 있습니다(kubectl과 대부분의 컨트롤러가 쓰는 경로).
  **API 서버의 직렬화 CPU와 네트워크 바이트를 JSON 대비 줄입니다.** 대시보드는 Pod·Event처럼
  객체 수가 많은 리소스를 통째로 watch하므로 이 차이가 그대로 클러스터 부하로 나타납니다.
  kube-rs는 이 경로가 없습니다 — protobuf 인코딩 지원은 오랫동안 열려 있던 과제이며,
  OpenAPI 스키마에 protobuf 필드 태그가 없다는 구조적 문제가 걸려 있습니다.
- **metadata-only informer.** client-go의 `metadatainformer`는 `PartialObjectMetadata`만 watch합니다.
  ReplicaSet처럼 소유 관계만 필요한 리소스는 spec/status를 받지 않아 전송량과 서버 직렬화 비용이
  더 줄어듭니다. 우리 화면에서 ReplicaSet은 OwnerReference 체인 표시에만 쓰이므로 바로 적용됩니다.
- **검증된 watch 세만틱.** reflector의 재연결·`resourceVersion` 처리·bookmark·410 Gone 복구·
  DeltaFIFO는 수년간 대규모 클러스터에서 검증된 구현입니다. 여기서 버그가 나면 증상이
  "대시보드가 느리다"가 아니라 **"대시보드가 API 서버에 LIST 폭풍을 일으킨다"** 입니다.
  이 코드를 직접 짜거나 덜 검증된 구현에 맡기는 것이 1순위 기준에서 가장 큰 위험입니다.
- **client-side rate limit과 Priority & Fairness 연동**이 기본 경로에 들어 있습니다.

### 2순위 — 백엔드 최적화에서 Rust의 우위가 결정적이지 않은 이유

- 이 서비스는 **I/O 바운드**입니다. 시간의 대부분은 watch 스트림 수신과 GreptimeDB·Quickwit 팬아웃
  대기입니다. CPU 바운드 구간은 캐시에서 읽은 객체를 DTO로 변환하는 정도입니다.
- 진짜 최적화는 언어가 아니라 설계에서 나옵니다 — 화면 단위 집계(ADR 0002), 인덱스,
  singleflight 중복 제거, TTL 캐시, 데이터소스별 타임아웃·서킷브레이커. 전부 Go에서 그대로 구현됩니다.
- Rust의 명확한 우위는 **메모리와 GC 지연**입니다. Pod 수만 개 규모의 informer 캐시에서
  Go는 힙이 더 크고 GC 일시정지가 생깁니다. 다만 이건 대시보드 응답 지연에 영향을 줄 뿐
  **클러스터 부하에는 영향이 없습니다.** 1순위 기준에 걸리지 않습니다.

### 함께 확정하는 구현 규칙

이 결정의 실익은 아래를 지킬 때만 유지됩니다. 언어보다 이 규칙이 중요합니다.

1. **요청 처리 경로에서 Kubernetes API를 호출하지 않습니다.** 항상 informer 캐시(lister)에서 읽습니다.
   요청당 API 서버 호출은 0회여야 합니다.
2. **폴링을 만들지 않습니다.** 자동 갱신은 브라우저 → 우리 백엔드까지만이고,
   백엔드 → API 서버는 watch 하나로 끝납니다. 사용자가 100명이어도 클러스터 부하는 같습니다.
3. **resync 주기를 짧게 두지 않습니다.** resync는 API 서버를 다시 치지 않지만 CPU를 씁니다. 기본 10분.
4. **필요한 리소스만 watch합니다.** 쓰지 않는 리소스에 informer를 붙이지 않습니다.
5. **ReplicaSet 등 관계만 필요한 리소스는 metadata-only informer**를 씁니다.
6. **내장 타입은 protobuf로 협상합니다.** CRD 등 protobuf가 없는 타입은 JSON으로 자동 폴백합니다.

## 검토한 대안

| 대안 | 기각 사유 |
|---|---|
| **Rust (kube-rs)** | 1순위 기준에서 지는 지점이 구체적입니다 — protobuf 협상 경로가 없어 API 서버 직렬화 비용과 전송량이 JSON에 묶입니다. watch 재연결·resourceVersion 처리 구현도 client-go보다 노출 사례가 적어, 잘못 다뤘을 때 대가가 "우리 서비스가 느려짐"이 아니라 "클러스터가 느려짐"입니다. 메모리·GC 우위는 2순위 기준에서만 유효합니다. |
| Rust + 직접 watch 구현 | 1순위 기준에서 가장 위험한 선택입니다. reflector 세만틱을 직접 구현하면 재연결 폭풍·중복 LIST 위험을 우리가 떠안습니다. |
| Node.js/TypeScript (프런트와 언어 통일) | `@kubernetes/client-node`의 informer는 client-go만큼 검증되지 않았고 protobuf도 없습니다. 단일 스레드 이벤트 루프에서 대규모 watch 처리와 팬아웃을 함께 감당하기 어렵습니다. |
| Java (fabric8) | informer 구현은 성숙하나 컨테이너 메모리 상주 비용이 크고 팀 스택과 멀어집니다. |
| kube-state-metrics만 조회 | 메트릭으로 정규화된 상태만 얻을 수 있어 Pod 이름·UID·OwnerReference 같은 식별 정보가 손실됩니다. Unified Entity Model(README §5)을 만들 수 없습니다. |

## 결과

**좋아지는 것**

- 대시보드 사용량과 무관하게 클러스터 부하가 상수입니다. 요청당 API 서버 호출 0회.
- protobuf + metadata-only watch로 API 서버 직렬화 비용과 전송량이 JSON 전체 객체 대비 줄어듭니다.
- watch 세만틱 버그로 클러스터를 흔들 위험을 검증된 구현에 넘깁니다.
- 이미 README §6과 이슈 #1/#8이 Go를 전제하고 있어 문서와 코드가 어긋나지 않습니다.

**감수하는 것**

- **메모리와 GC.** Pod 수만 개 캐시에서 Rust보다 힙이 크고 GC 일시정지가 생깁니다.
  완화책: metadata-only informer, 필요한 필드만 담는 내부 표현, `GOGC` 튜닝.
  이것이 문제가 되면 언어가 아니라 **캐시에 담는 표현**을 먼저 줄입니다.
- Go의 에러 처리와 제네릭 제약으로 어댑터 계층 코드가 Rust보다 장황해집니다.
- 이 결정은 **1순위 기준이 바뀌면 다시 볼 수 있습니다.** 클러스터 부하가 아니라
  단일 노드 처리량이 1순위가 되면 Rust가 유력합니다. 그때는 새 ADR을 씁니다.

## 후속 작업

- [x] **protobuf 협상이 실제로 적용되는지 확인.** 실제 kube-apiserver v1.31 대상 통합 테스트에서
      응답 Content-Type을 세어 확인했습니다 — protobuf 16건 · JSON 0건.
      (`TestLiveProtobufIsActuallyNegotiated`, `make api-itest`)
- [x] **요청당 API 서버 호출 0회를 실서버 트래픽으로 확인.** 화면 21회를 그리는 동안
      추가 호출 0회. (`TestLiveServingCausesZeroAPICalls`)
- [ ] informer 캐시 메모리 실측 후 metadata-only 적용 범위 확정 (이슈 #8)
- [ ] 대규모 클러스터(Pod 1만 이상)에서 초기 LIST 부하 측정 — WatchList(streaming list) 적용 여부 판단
- [ ] 데이터소스별 타임아웃·서킷브레이커 수치 확정 (이슈 #9)

## 참고

- [kube-rs — protobuf encoding exploration (#371)](https://github.com/kube-rs/kube/issues/371)
- [Kubernetes API Concepts — 콘텐츠 협상과 watch 세만틱](https://kubernetes.io/docs/reference/using-api/api-concepts/)
- [Kubernetes design proposal — protobuf 직렬화](https://github.com/kubernetes/design-proposals-archive/blob/main/api-machinery/protobuf.md)
