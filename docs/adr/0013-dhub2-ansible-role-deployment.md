# ADR 0013 — dhub2 Ansible role 기반 대시보드 배포와 모니터링 자동 연결

- 상태: Proposed
- 날짜: 2026-08-18
- 결정자: 미정
- 관련: Issue #26, #30, ADR 0005, dhub2 repo(멀티클러스터 Ansible 배포)

## 배경

대시보드는 조회 계층이므로 배포의 본질은 "이미지 + values + Secret + egress"다. 지금은
저장소 루트 `deploy.sh`(#26, #30)가 단일 클러스터 시험 배포를 담당하며, 대상 클러스터에서
GreptimeDB/Quickwit 서비스를 **런타임에 휴리스틱으로 발견**해 연결한다(#30).

그런데 사내 클러스터들(lnode 포함)의 관측 스택 자체가 dhub2 repo의 Ansible role
(`roles/greptimedb`, `roles/quickwit`, `roles/opentelemetry`)로 배포된다. 즉 데이터소스의
서비스 이름·네임스페이스·포트·로그 인덱스 스키마는 **role이 이미 알고 있는 선언적 사실**이고,
런타임 발견은 그 사실을 다시 추측하는 중복이다. dhub2에는 Secret 체계
(Infisical → external-secrets(ESO) → reloader)와 배포 단위(inventory per cluster,
`cluster_distro` 분기, ansible-vault secrets)가 이미 표준화되어 있다.

dhub2로 새 클러스터를 세울 때 대시보드와 모니터링 연결까지 자동으로 따라오게 하는 것이 목표다.

## 검토한 대안

| 대안 | 장점 | 단점 |
|---|---|---|
| A. dhub2 playbook이 `deploy.sh`를 command로 호출 | 구현 비용 0 | dhub2 규약(role/values j2/ESO) 이탈, 멱등성·체크모드 없음, WSL/도구 의존 |
| B. dhub2에 `roles/k8s-dashboard` 신설, 데이터소스는 role 변수로 선언적 연결 | dhub2 표준과 일치, 자동 발견이 필요 없어짐, Secret·rollout 자동화 재사용 | dhub2 repo 변경 필요, chart 버전 동기화 절차 필요 |
| C. GitOps(ArgoCD)로 chart 직접 동기화 | 선언적, 이력 자동 | dhub2의 현재 표준이 Ansible이라 이질적, 클러스터별 값 주입 체계 중복 |

## 결정

**B를 채택한다.** dhub2에 `roles/k8s-dashboard`(install/uninstall)를 추가하고,
이 저장소의 Helm chart(`deploy/helm/observability-dashboard`)를 role이 설치한다.

핵심 규칙:

1. **데이터소스 연결은 발견이 아니라 선언이다.** `GREPTIME_URL`/`QUICKWIT_URL`은
   dhub2 role 표준 서비스명(`greptimedb.greptimedb.svc:4000`,
   `quickwit-*.quickwit.svc:7280`)으로 values j2에서 렌더한다. Quickwit 로그 인덱스
   버전은 opentelemetry role과 **같은 inventory 변수 하나**(예: `otel_logs_index`)를
   정본으로 공유한다 — 스키마 버전이 오르면 두 곳이 함께 움직인다.
2. **Secret은 dhub2 체계를 그대로 탄다.** chart는 Secret을 만들지 않는 설계이므로
   (deploy/README.md), Infisical에 저장 → ExternalSecret이 `dashboard-datasources`
   Secret 렌더 → chart `api.existingSecret.name`으로 참조 → **reloader** annotation으로
   Secret 변경 시 자동 rollout. `secretRevision` 수동 증가를 대체한다.
   현재 lnode처럼 데이터소스가 무인증이면 ExternalSecret 블록은 조건부로 생략한다.
3. **배포판 분기는 `cluster_distro`로.** `okd`면 nonroot-v2 SCC RoleBinding과
   OpenShift Route를 함께 렌더한다(dhub2의 okd-scc·`*-okd-route.yaml.j2` 패턴).
   비-OKD는 Ingress/Gateway 경로를 쓴다.
4. **클러스터별 on/off는 inventory 변수**(`dashboard_enabled`)로 하고, playbook에는
   `tags: dashboard` 블록 하나만 추가한다. `CLUSTER_ID`/`CLUSTER_NAME`·scope도
   inventory 변수에서 렌더한다.
5. **`deploy.sh`는 폐기하지 않는다.** dhub2가 관리하지 않는 클러스터/로컬 시험 배포용
   경로로 유지하고, 자동 발견(#30)은 그 경로의 fallback으로 남는다.

chart 소스는 1차로 이 저장소를 서브트리/서브모듈 없이 **버전 태그가 박힌 차트 아카이브**
(dhub2 `charts/` 미러 규약)로 반입하고, 대시보드 릴리스 시 차트 버전을 올린다.

## 결과

| 유형 | 영향 |
|---|---|
| 기술 | 데이터소스 연결이 런타임 발견 → 선언(단일 변수 정본)으로 바뀐다. 인덱스 버전 상향은 opentelemetry role 변수 한 곳만 수정하면 대시보드가 따라간다. |
| 운영 | 새 클러스터 = inventory 작성 + `dashboard_enabled: true`. 모니터링 연결·SCC·Route·pull secret까지 playbook 1회로 끝난다. |
| 보안 | credential은 Infisical/ESO 밖으로 나오지 않고, chart의 "Secret은 운영자 소유" 계약이 유지된다. NetworkPolicy egress는 role이 아는 서비스 라벨로 정확히 조인다. |
| 장기 | OIDC 전환 시 `OIDC_CLIENT_SECRET`/`AUTH_SESSION_KEY`도 같은 ESO 경로에 얹는다. chart 버전 동기화(대시보드 릴리스 → dhub2 chart bump) 절차가 새로 생긴다. |

## 롤백 고려사항

- role은 uninstall task를 함께 제공한다(`helm uninstall` + 부속 RoleBinding/Route 제거,
  namespace는 보존). Secret은 ESO 소유이므로 ExternalSecret 삭제로 회수된다.
- 배포 실패 시 이전 chart 버전으로 role 변수만 되돌리면 된다(이미지 digest·values가
  inventory에 고정되므로 재현 가능).
- 이 ADR은 dhub2 repo 변경을 전제로 하며, 수락 전까지 이 저장소는 `deploy.sh` 경로를
  유지한다. 구체 뼈대는 `docs/dhub2/k8s-dashboard-role-skeleton.md` 참조.
