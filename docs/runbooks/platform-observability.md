# 플랫폼 자체 관측성 Runbook

`/metrics`는 Ingress에 노출하지 않고 API Service를 통해 Prometheus만 수집한다. API SLI는 probe와 SSE route를 제외하며 SSE 상태는 stream 전용 지표를 정본으로 사용한다. 로그에는 원문 경로·쿼리·Subject·토큰·upstream 오류를 남기지 않는다.

<a id="api-outage"></a>
## API 장애

확인: `absent(dashboard_http_requests_total)`과 `rate(dashboard_http_requests_total{status_class="5xx"}[5m])`를 확인하고 Pod readiness, 재시작 횟수, 최근 배포를 대조한다. 모든 route가 실패하면 API 자체 장애이며 특정 upstream outcome만 증가하면 데이터소스 장애다. 복구는 readiness 원인과 리소스 포화를 먼저 제거한다. 최근 배포가 원인이면 검증된 이전 chart revision과 API 이미지 digest로 롤백한다.

<a id="slow-api"></a>
## 느린 API

확인: `histogram_quantile(0.95, sum(rate(dashboard_http_request_duration_seconds_bucket[5m])) by (le,route))`. 동시에 upstream p95가 낮으면 API CPU/메모리, informer 조회, 응답 크기와 in-flight를 확인한다. upstream p95도 높으면 아래 데이터소스 절차를 따른다.

<a id="datasource-degradation"></a>
## 데이터소스 장애

확인: `sum(rate(dashboard_upstream_requests_total[5m])) by (upstream,outcome)`과 `dashboard_upstream_circuit_state`. timeout은 네트워크/부하/예산, bad_query는 배포된 query catalog 불일치, unavailable은 HTTP/응답 문제다. datasource만 복구하거나 마지막 catalog 변경을 롤백한다. 토큰, URL, 원문 query/body는 로그나 티켓에 복사하지 않는다.

<a id="informer-unsynced"></a>
## Informer 미동기화

확인: `dashboard_informer_synced == 0`, `/readyz`, Kubernetes API 도달성, ServiceAccount RBAC, LIST/WATCH 오류를 확인한다. RBAC 또는 API 네트워크 경계를 복구한 뒤 Pod를 순차 재시작하고 gauge가 1인지 확인한다.

<a id="query-protection-and-cache"></a>
## 질의 보호와 캐시

확인: `sum(rate(dashboard_query_rejected_total[5m])) by (reason)`과 `sum(rate(dashboard_cache_requests_total[5m])) by (result)`. hit ratio의 hit은 `l1_hit`, `redis_hit`, 동일 cold miss를 공유한 `coalesced`이며 `miss`, `miss_error`, `canceled`는 분모에만 포함한다. reject 급증은 클라이언트 폭주와 낮은 hit ratio를 함께 조사한다. Redis 오류에서도 bounded L1 fallback이 동작하는지 확인하고, 임의로 보호 한도를 해제하지 않는다.
