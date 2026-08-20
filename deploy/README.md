# Helm 배포

Issue #19의 배포 기준은 `deploy/helm/observability-dashboard` 단일 Helm chart입니다. 기존
`deploy/rbac/k8s-dashboard-api.yaml`은 informer 최소 권한의 검토용 기준이며, 실제 배포에서는
chart가 release-safe 이름으로 같은 규칙을 렌더합니다. Kustomize와 클러스터 적용은 이 범위에 없습니다.

## 사전 조건과 render-only 검증

- Kubernetes 1.29 이상(`preStop.sleep` lifecycle 사용)
- Docker(로컬 Helm 설치 불필요), Python 3
- 이미지 registry와 세 환경별 immutable digest
- stage/prod OIDC issuer/audience, Kubernetes API ClusterIP 또는 CIDR, 외부 데이터소스의 실제 CIDR/port
- 기존 Kubernetes Secret. chart는 Secret/ExternalSecret을 만들지 않습니다.

```bash
make deploy-images
make deploy-check

# 확인만 하는 개별 렌더 예시(클러스터 접속/apply 없음)
docker run --rm -v "$PWD:/work:ro" \
  alpine/helm:3.17.3@sha256:d899e6316789fec04ee95300a18e454b7942539cbb3d89bde3e0655d6ca2e895 \
  template dashboard /work/deploy/helm/observability-dashboard \
  --namespace observability -f /work/deploy/helm/observability-dashboard/values-prod.yaml
```

`values-stage.yaml`과 `values-prod.yaml`의 `registry.example.invalid` 및 예시 digest/CIDR/URL은
배포 전에 게시된 실제 이미지 digest와 환경 값으로 교체합니다. tag가 아니라 `repository@sha256:...`로
렌더되어야 합니다. CI는 build만 하고 push나 apply를 수행하지 않습니다.

## 환경 차이

| 항목 | dev | stage | prod |
|---|---|---|---|
| UI/API | 1 replica, HPA/PDB off | 2+, HPA/PDB on | 2+, HPA/PDB on |
| 인증/데이터 | `none`, demo 허용 | OIDC, demo 금지 | OIDC, demo 금지 |
| Redis | single pod + `emptyDir` | single pod + PVC | 기본 off, 기존 `REDIS_ADDR` Secret |
| 이미지 | 개발 tag 허용 | digest 필수 | digest 필수 |

bundled Redis는 HA/replication/operator가 없는 단일 장애점입니다. stage PVC는 재시작 간 데이터를
보존하지만 가용성을 높이지 않습니다. prod는 기본적으로 외부 Redis 주소만 `REDIS_ADDR` Secret key로
받습니다. 애플리케이션의 Redis auth/TLS 기능은 추가하지 않았습니다.

브라우저 세션을 외부 Redis와 사용할 때는 `networkPolicy.external`에 `purpose: redis`인 목적지를
정확히 하나 선언해야 합니다. chart는 Secret의 `REDIS_ADDR` host/port를 읽지 않으므로 운영자가 이
egress CIDR/port와 Secret 값을 일치시켜야 하며, 불일치하면 API 기동/readiness가 fail closed됩니다.
bundled Redis를 사용하는 경우에는 별도 external Redis egress가 필요하지 않습니다.

## 기존 Secret 계약

`api.existingSecret.name`은 이미 존재하는 Secret 이름이며 다음 key 이름만 참조합니다.

- `GREPTIME_USERNAME`, `GREPTIME_PASSWORD`
- `QUICKWIT_USERNAME`, `QUICKWIT_PASSWORD`
- `REDIS_ADDR` (`host:port`, bundled Redis를 끈 경우 필수)

값은 chart/values/render 결과에 포함하지 않습니다. Secret 내용 변경은 Deployment를 자동 재시작하지
않으므로 외부 reloader를 사용하거나 `secretRevision`을 증가시켜 rollout을 유도합니다. OIDC issuer와
audience는 비밀이 아니므로 ConfigMap에 둡니다.

## 브라우저 세션과 OIDC (ADR 0011)

브라우저 로그인은 `authSession.enabled=true`로 켭니다. 기본 검증은 chart Ingress
(TLS secret 포함)를 요구하지만, OpenShift Route나 외부 LB가 TLS 게시를 맡는 배포는
`authSession.externalIngress=true`로 선언합니다. 이때도 `publicOrigin`은 경로 없는
HTTPS여야 하고 `redirectURI`는 정확히 `<publicOrigin>/api/v1/auth/callback`이어야
하며, `api.existingSecret`에 32-byte base64url(무패딩) `AUTH_SESSION_KEY`가 필요합니다.

루트의 `deploy.sh`는 **OIDC가 기본**입니다(`AUTH_MODE=none`으로만 옵트아웃).
클러스터의 keycloak Route와 기존 `<release>-ui` Route를 자동 발견해 issuer와
공개 origin을 구성하고, `AUTH_SESSION_KEY`가 없으면 생성하며, issuer로의
`purpose: oidc` egress를 주입합니다. IdP에는 다음이 한 번 준비되어 있어야 합니다.

- public client(권장) + PKCE S256, redirect URI = `<publicOrigin>/api/v1/auth/callback`
- ID/access token에 **최상위 flat `roles` 배열** claim (중첩 `realm_access.roles`는
  파싱되지 않습니다 — Keycloak은 client-role 매퍼에 `claim.name=roles`를 지정)
- 로그인할 계정에 client role 부여(`platform.admin`, `namespace.viewer:<ns>` 등)
- refresh 응답에 서명된 ID token 포함(Keycloak 충족 — 미지원 provider는 세션이
  fail-closed 됩니다)

deploy.sh의 issuer 자동 발견은 **dhub2-auth(DHubManager 내장 OIDC provider) Route를
우선**하고, 없으면 keycloak Route로 fallback합니다. dhub2-auth를 쓰면 플랫폼 계정을
그대로 사용하며, `type=admin` 사용자의 토큰 `groups` 클레임에 실리는 `dhub2-admin`이
`OIDC_ROLE_MAP=dhub2-admin=platform.admin` 기본값으로 대시보드 전체 접근으로 해석됩니다.

lnode 시험 클러스터: dhub2-manager(`manager.apps.okd.dtonic.io`)에 public PKCE client
`k8s-dashboard`가 2026-08-20에 등록되어 있습니다(Keycloak dhub2 realm의 동명 client는
전환기 임시 구성으로, dhub2-auth 안정화 후 제거 대상입니다).

## Dashboard Builder PostgreSQL

Dashboard Builder는 기본 `enabled=false`이며 disabled render에는 DB env/egress가 추가되지 않습니다. 활성화할 때는 `api.existingSecret.name`, `dashboardBuilder.databaseURLKey`, `cursorKeyKey`, `postgresEgress.cidrs/port`를 환경 소유 값으로 지정합니다. chart는 PostgreSQL을 배포하지 않으며 Secret의 URL을 출력하지 않습니다.
stage/prod에서는 `dashboardBuilder.requireTLS=true`와 `sslmode=verify-full` DSN, 신뢰 CA 및 일치하는 DB hostname이 필수입니다. NetworkPolicy port와 Secret DSN port는 chart가 Secret을 읽을 수 없으므로 운영자가 동일하게 유지하며, 불일치는 API 시작 또는 readiness 503으로 진단합니다.

## NetworkPolicy 제한

기본 deny 후 UI→API, API→Kubernetes API/Redis/선언된 데이터소스, DNS만 엽니다. Ingress는 UI
Service만 대상으로 합니다. 표준 NetworkPolicy는 FQDN, Service 이름, HTTP host/path를 표현하지 못하므로
OIDC처럼 IP가 바뀌는 목적지는 CNI의 FQDN 정책을 별도로 사용하거나 실제 egress CIDR을 운영 절차로
갱신해야 합니다. `kubernetesApiCidrs`에는 실제 `KUBERNETES_SERVICE_HOST` ClusterIP/CIDR을 넣습니다.
ingress-controller 및 monitoring selector는 설치 환경 label과 일치해야 합니다. 일부 CNI의 kubelet
probe 출발점 처리가 다르므로 배포 전 해당 CNI에서 `/healthz`와 `/readyz`를 확인합니다. stage/prod의
`0.0.0.0/0` 및 `::/0`은 정책 검사에서 거부합니다.

## 롤백

실행 시에는 이전 values revision과 image digest를 보관합니다. 장애 시 `helm rollback <release> <revision>`
또는 이전 digest/values로 upgrade합니다. Secret 자체는 외부 관리 대상이므로 별도 revision과 함께
되돌립니다. 이 변경에서는 실제 cluster apply나 rollback을 수행하지 않았습니다.
# 플랫폼 모니터링 자산

`networkPolicy.monitoring.enabled=true`일 때만 API Service scrape annotation, Prometheus ingress NetworkPolicy, Grafana sidecar-discovery ConfigMap이 함께 생성됩니다. Grafana JSON의 단일 정본은 chart의 `files/dashboard.json`입니다. Alert rule은 CRD를 생성하지 않는 repo-owned 자산이며 운영 Prometheus rule loader가 `deploy/monitoring/alerts.yaml`을 별도로 소유·설치합니다.

## OpenTelemetry 수집 전환

`telemetry.mode` 기본값은 `disabled`이며 기존 dev/stage/prod render를 바꾸지 않습니다. `validate`는 backend write 없이 Agent/singleton cluster collector→Gateway 경로만 검증하고, `cutover`는 기존 signal별 collector 중지 승인과 실제 comparison evidence가 있어야 render됩니다.

- 설계와 위협/실패 흐름: [`docs/telemetry/architecture.md`](../docs/telemetry/architecture.md)
- 현재 source/query/schema inventory: [`docs/telemetry/current-path-inventory.md`](../docs/telemetry/current-path-inventory.md)
- preflight·측정·cutover·rollback: [`docs/telemetry/cutover-runbook.md`](../docs/telemetry/cutover-runbook.md)
- 결정 상태: [ADR 0008](../docs/adr/0008-opentelemetry-agent-gateway-pipeline.md) — Proposed

stage/prod values에는 `check-telemetry-evidence.py --helm-values-out`이 생성한 comparison overlay만 사용합니다. Helm은 hash의 진위를 자체 검증하지 못하므로 수기 복사한 측정값은 운영 cutover evidence로 인정하지 않습니다.
