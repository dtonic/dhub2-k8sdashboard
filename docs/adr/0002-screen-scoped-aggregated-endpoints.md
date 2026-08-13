# ADR 0002 — 화면 단위 집계 엔드포인트를 BFF의 기본 형태로 삼는다

- 상태: Accepted
- 날짜: 2026-08-13
- 결정자: @xenx96
- 관련: 이슈 [#14](https://github.com/xenx96/k8s-dashboard/issues/14), [#13](https://github.com/xenx96/k8s-dashboard/issues/13) · README §11 성능 원칙 · ADR 0001

## 배경

Cluster Overview 한 화면에는 Node 상태, Pod 상태, Workload replica, CPU·Memory·Network·Restart 추세,
이상 엔티티 Top N, 최근 Event, 활성 Alert, 통신 경로 요약이 함께 놓입니다. 위젯을 기준으로 API를 나누면
첫 진입에서만 8~10회 요청이 나가고, Namespace 필터를 바꿀 때마다 그 수만큼 다시 나갑니다.

이슈 #14의 완료 기준은 이 문제를 직접 지목합니다 — **"초기 로딩에서 N+1 API 요청이 발생하지 않음."**

동시에 지켜야 하는 조건이 있습니다.

1. 한 데이터소스(GreptimeDB / Quickwit / Kubernetes API / Alertmanager)가 죽어도 나머지 화면은 살아야 합니다.
2. "데이터 없음", "권한 없음", "upstream 장애"가 화면에서 구분되어야 합니다.
3. 권한 Scope는 서버가 강제해야 합니다. UI가 보낸 cluster/namespace를 신뢰하지 않습니다.

리소스 단위 REST(`/pods`, `/nodes`, `/events` …)로는 1번과 2번이 클라이언트 조합 로직으로 새어 나갑니다.
화면마다 "어떤 조합이 부분 장애인가"를 프런트엔드가 다시 판단하게 됩니다.

## 결정

**BFF는 리소스가 아니라 화면을 단위로 응답한다.**

1. 화면 하나당 엔드포인트 하나. Cluster Overview는 `GET /api/v1/clusters/{clusterId}/overview` 하나로 끝납니다.
   Scope Selector가 쓰는 `GET /api/v1/scope`만 예외로 분리합니다(변경 빈도와 캐시 수명이 다름).
2. 응답은 **섹션 봉투**로 감쌉니다.

   ```ts
   interface Section<T> {
     status: "ok" | "empty" | "forbidden" | "degraded";
     data?: T;
     source?: "greptimedb" | "quickwit" | "kubernetes" | "alertmanager";
     reason?: string;
     observedAt?: string;
   }
   ```

   부분 장애는 예외가 아니라 **값**입니다. degraded인데 마지막 성공 값이 있으면 함께 내려보내고,
   화면은 stale 표시와 함께 계속 보여줍니다.
3. **권한 부족은 에러가 아니라 상태입니다.** 섹션 단위 `forbidden`으로 내려오고, 빈 결과와 다르게 렌더링합니다.
   화면 전체에 대한 접근 거부만 HTTP 403입니다.
4. 응답에 **서버가 실제로 적용한 Scope**(`appliedScope`)를 포함합니다. UI가 요청한 값과 다를 수 있으며,
   화면은 적용된 값을 표시합니다.
5. Step은 응답에 포함되며 서버가 정합니다. 클라이언트는 고르지 않고 표시만 합니다.
6. 계약은 `packages/contracts`에 TypeScript로 두고 UI와 공유합니다. 추후 OpenAPI 생성으로 대체할 수 있습니다.

## 검토한 대안

| 대안 | 기각 사유 |
|---|---|
| 리소스 단위 REST + 클라이언트 조합 | N+1이 구조적으로 발생한다. 부분 장애 판단과 상태 구분이 화면마다 중복 구현된다. |
| GraphQL | 필요한 것은 클라이언트가 임의로 질의를 조립하는 자유가 아니라 **서버가 쿼리 비용을 강제하는 것**이다. 임의 질의 조합은 Query Catalog 원칙(README §2-4)과 정면으로 충돌하고, 비용 상한을 걸기 어렵다. |
| 단일 만능 엔드포인트(`/dashboard?widgets=…`) | 화면별 응답 형태가 런타임에 달라져 타입 계약이 무의미해진다. 캐시 키도 폭발한다. |
| 위젯 단위 엔드포인트 + HTTP/2 멀티플렉싱 | 요청 수 자체는 감춰지지만 데이터소스 fan-out과 인증·Scope 검사가 요청마다 반복된다. 서버 부하는 그대로다. |
| 초기 로드만 집계 + 갱신은 위젯별 | 갱신 경로가 두 벌이 되어 부분 장애 처리가 어긋난다. 갱신도 같은 엔드포인트를 쓴다. |

## 결과

**좋아지는 것**

- 첫 진입 API 요청 2건(`/scope` + `/overview`). 범위·Scope 변경 시 1건.
- 부분 장애 판단이 서버 한 곳에 모인다. 화면은 봉투 상태만 보고 그린다.
- Scope 강제와 쿼리 가드레일이 엔드포인트 한 곳에서 이뤄진다. 우회 경로가 생기지 않는다.
- 캐시 키가 `(clusterId, namespace, range)`로 단순해진다. 사용자 권한 Scope를 캐시 키에 포함하기 쉽다.

**감수하는 것**

- 화면이 늘면 엔드포인트도 는다. 화면과 API의 결합이 의도적으로 강해진다 —
  BFF는 원래 그런 계층이고, 범용 API가 필요하면 별도 레이어로 분리한다.
- 응답 하나가 커진다. 섹션별 필드 선택(`?sections=`)이 필요해지면 그때 추가한다. 지금은 넣지 않는다.
- 서버가 여러 데이터소스를 fan-out 하므로 타임아웃·Circuit Breaker를 데이터소스별로 걸어야 한다.
  가장 느린 소스가 화면 전체를 잡아먹지 않도록 섹션 단위 데드라인이 필요하다.

## 후속 작업

- [ ] 섹션 단위 데드라인과 Circuit Breaker 정책 수치 확정 (이슈 #9)
- [ ] 사용자 권한 Scope를 포함한 캐시 키 규칙 문서화 (이슈 #10)
- [ ] `packages/contracts`를 OpenAPI 생성으로 전환할지 결정 (이슈 #4)
- [ ] Namespace/Workload 상세(#15), Logs Explorer(#16)도 같은 형태를 따르는지 검토
