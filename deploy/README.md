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

## 기존 Secret 계약

`api.existingSecret.name`은 이미 존재하는 Secret 이름이며 다음 key 이름만 참조합니다.

- `GREPTIME_USERNAME`, `GREPTIME_PASSWORD`
- `QUICKWIT_USERNAME`, `QUICKWIT_PASSWORD`
- `REDIS_ADDR` (`host:port`, bundled Redis를 끈 경우 필수)

값은 chart/values/render 결과에 포함하지 않습니다. Secret 내용 변경은 Deployment를 자동 재시작하지
않으므로 외부 reloader를 사용하거나 `secretRevision`을 증가시켜 rollout을 유도합니다. OIDC issuer와
audience는 비밀이 아니므로 ConfigMap에 둡니다.

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
