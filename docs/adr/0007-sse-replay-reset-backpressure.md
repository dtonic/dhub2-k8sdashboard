# ADR 0007 — SSE 상태 변경 스트림은 프로세스 로컬 재생 링 · reset 폴백 · 연결 절단 backpressure로 유계를 유지한다

- 상태: Accepted
- 날짜: 2026-08-14
- 결정자: @xenx96
- 관련: 이슈 [#12](https://github.com/xenx96/k8s-dashboard/issues/12) · ADR 0004, ADR 0005 · README §10·§11

## 배경

ADR 0005는 Pod·Workload·Kubernetes Event·Alert 상태 변경을 SSE로, 시계열 Metrics는 HTTP로
전달하기로 확정했습니다. 남은 것은 전달 계층의 구체 정책입니다 — 재연결 시 유실 복구,
느린 구독자 처리, Scope 격리, 아직 실클라이언트가 없는 Alert 소스, 그리고 이 모든 것의
메모리·연결 상한입니다. 어느 것도 informer 콜백을 막거나(ADR 0004) 무한 버퍼를 만들면 안 됩니다.

## 결정

**스트림은 무효화 신호만 나릅니다.** 봉투(EventEnvelope)에는 kind·action·UID 우선
EntityRef·resourceVersion만 싣고, Kubernetes 원본 객체·Alert annotation·시계열 샘플은 싣지
않습니다. 데이터 본문은 기존 화면 단위 HTTP 계약이 담당합니다.

구체 정책은 다섯 가지입니다.

1. **프로세스 로컬 재생 링.** 이벤트 ID는 `<인스턴스 hex>-<단조 시퀀스>`이고, 고정 크기
   순환 버퍼(`STREAM_REPLAY_EVENTS`)가 최근 이벤트를 보존합니다. 같은 인스턴스·보존 구간 안의
   Last-Event-ID면 놓친 이벤트를 순서대로 재생합니다. 재생 절단과 구독 등록은 같은 잠금
   안에서 일어나므로 재생과 라이브 사이에 틈도 중복도 없습니다.
2. **복구를 확신할 수 없으면 reset.** 다른 인스턴스의 ID(재시작·replica 이동), 보존 구간 밖,
   미래 시퀀스는 조용히 이어붙이지 않고 `kind=reset` 제어 이벤트로 답합니다. 브라우저는 reset을
   받으면 현재 상태를 HTTP로 다시 조회합니다. 형식이 틀리거나 상한(64자)을 넘는 Last-Event-ID는
   구독 자원 배정 전에 400으로 거절합니다.
3. **backpressure는 연결 절단.** Publish는 논블로킹·O(활성 연결 수)입니다. 구독자 채널
   (`STREAM_SUB_BUFFER`)이 가득 차면 그 구독자를 즉시 끊습니다 — informer 콜백을 세우거나
   버퍼를 늘리는 선택지는 없습니다. 끊긴 브라우저는 Last-Event-ID 재연결로 복구합니다(1·2번).
4. **Scope는 전달 시점마다 강제.** 라이브·재생 모두 서버가 해석한 Scope로 필터링합니다.
   namespace 사용자는 다른 namespace 봉투를 받지 않고, namespace 없는 클러스터 범위 신호는
   전체(All) Scope에만 갑니다. reset은 클러스터 접근 권한만 있으면 받는 제어 신호입니다.
   요청 주체는 전역·주체별 연결 상한(`STREAM_MAX_CONNECTIONS`·`STREAM_MAX_PER_SUBJECT`)에만
   쓰입니다. SSE는 #11 질의 보호(12s budget·rate·cache)의 대상이 아닙니다 — 연결은 질의가
   아니기 때문이고, 대신 위 상한과 write 유휴 데드라인(`STREAM_WRITE_IDLE`)이 자원을 지킵니다.
5. **Alert 소스는 유계 스냅숏 diff.** 운영 Alertmanager와 demo가 공통
   `datasource.Alerts` 추상화를 `ALERT_POLL_INTERVAL` 주기로 조회해 스냅숏(상한
   `ALERT_SNAPSHOT_MAX`) 차이만 발행합니다. 최초 스냅숏은 발행하지 않고, 실패는 지수 backoff
   (`ALERT_POLL_MAX_BACKOFF`)로 물러납니다. 이 폴링은 데이터소스 방향이며 Kubernetes API
   폴링 금지(ADR 0004)와 무관합니다.

## 검토한 대안

| 대안 | 기각 사유 |
|---|---|
| Redis 등 외부 브로커에 이벤트 로그를 두고 인스턴스 간 재생 | MVP는 프로세스당 단일 클러스터이며, cross-replica 재연결은 reset을 감수합니다. 브로커는 새 장애 지점과 운영 부담을 더하고, reset 폴백만으로 정확성이 보장됩니다. |
| 느린 구독자를 위해 버퍼를 동적으로 늘리기 | 느린 소비자가 서버 메모리를 정합니다. 무유계 큐는 README §11 위반이고, 끊고 재생하는 쪽이 항상 유계입니다. |
| 유실 시 reset 대신 조용히 최신 이벤트부터 이어붙이기 | 화면이 놓친 변경을 영영 모릅니다. "완전 재생 아니면 명시적 reset"이 유일하게 정직한 계약입니다. |
| Alert도 informer처럼 watch | Alertmanager는 watch API가 없습니다. 폴링 주기·스냅숏 상한이 있는 diff가 현재 추상화로 가능한 최선입니다. |
| WebSocket | 단방향 알림에 양방향 프로토콜은 과합니다. ADR 0005가 이미 기각했습니다. |

## 결과

**좋아지는 것** — 메모리·연결·재생 창이 전부 구조적 상한을 갖습니다. informer 콜백의
critical section은 유계이며 slow-client I/O를 기다리지 않습니다. 재연결 복구가 "재생 또는
reset"으로 단순하고 검증 가능합니다.

**감수하는 것** — 재생은 프로세스 로컬입니다. 재시작·replica 이동 시 구독자 전원이 reset을
받아 HTTP 재조회가 몰립니다(캐시 #11이 흡수). 링 크기보다 오래 끊긴 클라이언트도 reset입니다.
Alert 변경은 폴링 주기만큼 늦습니다. 운영 Alertmanager는 명시적으로 활성화한 경우에만
실소스가 되며, 비활성화 상태에서는 데모(USE_DEMO_DATA) 또는 조용한 degraded입니다.

## 후속 작업

- [x] Alertmanager 현재 알림을 `datasource.Alerts`로 연결해 Alert 스트림 실소스 확보
      (Resolved 운영 이력은 ADR 0012의 P2 후속)
- [ ] Multi-replica 배포가 확정되면 sticky session 또는 공유 이벤트 로그 재검토 (Post-MVP)
- [ ] 제품 UI의 authenticated fetch-stream·reset 재조회 배선 (별도 Web-API 실연결 후속;
      #12 백엔드 프로토콜·재접속 의미는 실제 HTTP SSE 테스트로 검증됨)
