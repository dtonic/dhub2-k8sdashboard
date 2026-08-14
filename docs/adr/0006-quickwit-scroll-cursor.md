# ADR 0006 — Quickwit 로그 페이징에 TTL scroll capability를 사용한다

- 상태: Accepted
- 날짜: 2026-08-14
- 결정자: @xenx96
- 관련: 이슈 [#7](https://github.com/xenx96/k8s-dashboard/issues/7) · ADR 0003 대체

## 배경

ADR 0003의 timestamp와 경계 id 집합 방식은 같은 밀리초에 512건이 넘으면 중복될 수 있고,
집합을 `MaxLines`까지 늘리면 실제 700건에서 GET cursor가 20,124바이트가 된다. Quickwit은
자체 `_id`를 제공하지 않으며, 안정적인 secondary sort는 기존 인덱스에 unique fast field를
추가해야 한다. 이는 기존 수집 스키마와 호환되지 않는다.

## 결정

- 최초 조회는 서버가 만든 Scope·window·filter와 `scroll=1m`으로 Quickwit snapshot을 만든다.
- 다음 조회는 Quickwit이 돌려준 `scroll_id`를 `_search/scroll` GET body로 보낸다. offset은 쓰지 않는다.
- cursor에는 scroll id, 누적 반환·scan 수, total, query digest, initial-search nonce를 담고 Source 생성 시
  `crypto/rand`로 만든 256-bit process-local key로 HMAC-SHA256 서명한다. 현재 서버 Scope/query digest가
  다르거나 HMAC이 틀리면 upstream 요청 전에 거절한다. encoded cursor는 8KiB를 hard limit로 둔다.
- 정상·비정상 hit을 모두 누적 scan 수에 포함하고 `MaxLines`를 넘기지 않는다. 고정 scroll page가 남은
  budget보다 크면 호출하지 않아 최대 page size-1만큼 underfill할 수 있다. 뒤에 데이터가 있으면
  `truncated=true`, `next`는 비우며 scan 상한 안의 정상 line만 반환한다.
- 선택적 stored `event_id`가 있으면 `LogLine.ID`로 쓴다. 없으면 Source key, initial nonce, snapshot의
  absolute ordinal로 traversal 범위에서 고유한 opaque ID를 만든다. refresh 간 안정성은 보장하지 않는다.

## 검토한 대안

| 대안 | 결과 |
|---|---|
| timestamp + boundary fingerprint | 정확한 집합은 8KiB를 넘고 확률적 fingerprint는 충돌 가능성이 있다. |
| timestamp + unique fast field search-after | 실 API에서 정확히 동작하지만 기존 인덱스·수집 schema migration이 필요하다. |
| Quickwit `_id` secondary sort | Quickwit은 document id 개념을 지원하지 않으며 실 응답에도 stable `_id`가 없다. |
| offset/page | 실시간 로그에서 중복·누락이 발생하므로 금지한다. |

## 결과와 위험

- 실 Quickwit 0.8.2에서 동일 timestamp 700건을 중복·누락 없이 순회했고 scroll id는 최대
  232바이트, HMAC envelope까지 포함한 API cursor는 최대 540바이트였다. 기존 인덱스 schema를 바꾸지 않는다.
- scroll context는 TTL 상태다. Quickwit 0.8.2는 clear DELETE를 405로 거절하므로 중단한 조회는 1분 뒤
  만료된다. TTL 뒤 재사용은 `scroll key not found`로 실패한다.
- process-local key 때문에 API 재시작 또는 replica 변경 뒤 기존 cursor는 거절된다. 현재 단일 Source
  MVP에는 안전하지만 다중 replica에서는 sticky routing 또는 공유 key 정책을 별도 결정해야 한다.
- `event_id` 없는 fallback ID는 현재 누적 traversal 안에서만 안정적이다. UI는 React key/open row에만
  사용하며 refresh 간 신원을 요구하지 않는다.
- scroll 재호출의 멱등성이 보장되지 않으므로 후속 scroll GET은 자동 retry하지 않는다.

## 후속 작업

- [x] 보안·운영 리뷰 후 Accepted
- [ ] 다중 replica cursor key와 sticky routing 정책 결정
- [ ] 수집 파이프라인이 stable `event_id`를 제공할 수 있는지 검토
