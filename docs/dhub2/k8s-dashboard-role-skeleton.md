# dhub2 `roles/k8s-dashboard` 뼈대 (ADR 0013 부속)

dhub2 repo에 그대로 옮겨 붙일 수 있는 role 뼈대입니다. dhub2 규약
(install/uninstall 분리, `kubernetes.core.k8s` + helm, `*-values.yaml.j2`,
`cluster_distro` 분기, ESO/Infisical/reloader)을 따랐습니다.
**이 문서는 제안이며, dhub2 반영은 해당 repo의 리뷰 절차를 따릅니다.**

<details>
<summary>펼치기 — 디렉터리 구조</summary>

```text
roles/k8s-dashboard/
├── README.md
├── defaults/
│   └── main.yaml
├── tasks/
│   └── main.yaml
├── templates/
│   ├── dashboard-values.yaml.j2
│   ├── dashboard-external-secret.yaml.j2   # credential 있는 환경에서만 렌더
│   ├── dashboard-okd-scc.yaml.j2           # cluster_distro == 'okd'
│   └── dashboard-okd-route.yaml.j2         # cluster_distro == 'okd'
└── uninstall/
    └── tasks/
        └── main.yaml
```

</details>

## 1. defaults/main.yaml

변수 정본 규칙:

- 데이터소스 주소는 **dhub2 role 표준 서비스명**으로 기본값을 둔다 (발견 아님).
- `dashboard_quickwit_index`의 기본값은 opentelemetry role의 인덱스 변수
  (`otel_logs_index`)를 참조한다 — 스키마 버전의 정본은 한 곳이다.

<details>
<summary>펼치기 — defaults/main.yaml</summary>

```yaml
# 클러스터별 on/off — inventory에서 dashboard_enabled: true로 켠다.
dashboard_enabled: false

dashboard_namespace: observability
dashboard_release: dashboard

# chart는 dhub2 charts/ 미러 규약으로 반입한 버전을 쓴다.
dashboard_chart: "{{ charts_dir }}/observability-dashboard-{{ dashboard_chart_version }}.tgz"
dashboard_chart_version: 0.1.0

# 이미지 — 폐쇄망 클러스터는 registry.hub.dtonic.io 미러로 override.
dashboard_image_registry: registry.hub.dtonic.io/lnode
dashboard_web_image: "{{ dashboard_image_registry }}/observability-dashboard-web"
dashboard_api_image: "{{ dashboard_image_registry }}/observability-dashboard-api"
dashboard_image_tag: ""        # 릴리스 태그 필수 지정
dashboard_image_digest: ""     # stage/prod는 digest 권장 (chart 규칙)

# ── 데이터소스: dhub2 role 표준 서비스명 (선언적 연결, ADR 0013 §1) ──
dashboard_greptime_url: http://greptimedb.greptimedb.svc.cluster.local:4000
dashboard_greptime_db: public
dashboard_quickwit_url: http://quickwit-searcher.quickwit.svc.cluster.local:7280
# 로그 인덱스 버전의 정본은 opentelemetry role — 같은 변수를 참조한다.
dashboard_quickwit_index: "{{ otel_logs_index | default('otel-logs-v0_9') }}"
dashboard_quickwit_fields: >-
  timestamp=timestamp_nanos,level=severity_text,message=body.message,
  namespace=resource_attributes.k8s.namespace.name,
  pod_name=resource_attributes.k8s.pod.name,
  pod_uid=resource_attributes.k8s.pod.uid,
  container=resource_attributes.k8s.container.name,
  workload_name=resource_attributes.k8s.deployment.name,
  node=resource_attributes.k8s.node.name,trace_id=trace_id,span_id=span_id

# NetworkPolicy egress용 — 데이터소스 서비스의 spec.selector와 일치해야 한다.
dashboard_greptime_pod_labels: {app.kubernetes.io/name: greptimedb}
dashboard_quickwit_pod_labels: {app: quickwit, service: searcher}

# ── Secret (ADR 0013 §2) — 무인증 환경이면 빈 값으로 두면 ESO 블록이 생략된다 ──
dashboard_secret_name: ""                 # 예: dashboard-datasources
dashboard_infisical_path: /dashboard      # Infisical 내 시크릿 경로
dashboard_secret_keys: []                 # 예: [GREPTIME_USERNAME, GREPTIME_PASSWORD]

# 노출 — okd는 Route, 그 외는 chart ingress를 쓴다.
dashboard_route_host: ""                  # 예: k8sdashboard.apps.okd.dtonic.io

# scope — 운영 초기에는 관측 대상 namespace만 연다.
dashboard_cluster_id: "{{ k8s_context }}"
dashboard_cluster_name: "{{ k8s_context }}"
dashboard_scope_namespaces: "*"
dashboard_auth_mode: none                 # 운영 전환 시 oidc (ADR 0011)
```

</details>

## 2. tasks/main.yaml

순서가 중요합니다: namespace → (okd) SCC → (선택) ExternalSecret 대기 → helm → (okd) Route.
SCC RoleBinding이 helm보다 먼저여야 첫 Pod가 거부되지 않습니다(#26에서 검증된 순서).

<details>
<summary>펼치기 — tasks/main.yaml</summary>

```yaml
- name: Skip when dashboard is disabled for this cluster
  ansible.builtin.meta: end_play
  when: not dashboard_enabled

- name: Create dashboard namespace
  kubernetes.core.k8s:
    state: present
    definition:
      apiVersion: v1
      kind: Namespace
      metadata:
        name: "{{ dashboard_namespace }}"

# chart가 runAsUser(101/65532/999)를 고정하므로 OKD restricted-v2가 거부한다.
# helm 설치 전에 nonroot-v2 사용 권한을 release ServiceAccount에 부여한다.
- name: Grant nonroot-v2 SCC to dashboard service accounts (OKD)
  kubernetes.core.k8s:
    state: present
    apply: true
    definition: "{{ lookup('template', 'dashboard-okd-scc.yaml.j2') | from_yaml }}"
  when: cluster_distro == 'okd'

- name: Create dashboard datasource ExternalSecret
  kubernetes.core.k8s:
    state: present
    apply: true
    definition: "{{ lookup('template', 'dashboard-external-secret.yaml.j2') | from_yaml }}"
  when: dashboard_secret_name | length > 0

- name: Wait for dashboard ExternalSecret to be Ready
  ansible.builtin.include_role:
    name: external-secrets/wait
  vars:
    eso_wait_namespace: "{{ dashboard_namespace }}"
    eso_wait_name: "{{ dashboard_secret_name }}"
  when: dashboard_secret_name | length > 0

- name: Install observability dashboard
  kubernetes.core.helm:
    name: "{{ dashboard_release }}"
    chart_ref: "{{ dashboard_chart }}"
    release_namespace: "{{ dashboard_namespace }}"
    values: "{{ lookup('template', 'dashboard-values.yaml.j2') | from_yaml }}"
    wait: true

- name: Expose dashboard route (OKD)
  kubernetes.core.k8s:
    state: present
    apply: true
    definition: "{{ lookup('template', 'dashboard-okd-route.yaml.j2') | from_yaml }}"
  when: cluster_distro == 'okd' and dashboard_route_host | length > 0
```

</details>

## 3. templates/dashboard-values.yaml.j2

chart values의 정본은 이 j2 하나입니다. scripts/deploy.sh(#30)가 런타임에 만들던 overlay를
선언으로 옮긴 것과 같습니다.

<details>
<summary>펼치기 — dashboard-values.yaml.j2</summary>

```yaml
environment: dev            # 운영 전환 시 stage/prod + digest 필수
fullnameOverride: {{ dashboard_release }}
ui:
  image:
    repository: {{ dashboard_web_image }}
    tag: {{ dashboard_image_tag }}
    digest: "{{ dashboard_image_digest }}"
api:
  image:
    repository: {{ dashboard_api_image }}
    tag: {{ dashboard_image_tag }}
    digest: "{{ dashboard_image_digest }}"
  config:
    CLUSTER_ID: "{{ dashboard_cluster_id }}"
    CLUSTER_NAME: "{{ dashboard_cluster_name }}"
    AUTH_MODE: "{{ dashboard_auth_mode }}"
    SCOPE_NAMESPACES: "{{ dashboard_scope_namespaces }}"
    GREPTIME_URL: "{{ dashboard_greptime_url }}"
    GREPTIME_DB: "{{ dashboard_greptime_db }}"
    QUICKWIT_URL: "{{ dashboard_quickwit_url }}"
    QUICKWIT_INDEX: "{{ dashboard_quickwit_index }}"
    QUICKWIT_FIELDS: "{{ dashboard_quickwit_fields | replace('\n', '') }}"
{% if dashboard_secret_name %}
  existingSecret:
    name: {{ dashboard_secret_name }}
{% endif %}
redis:
  image: {repository: docker.io/library/redis}   # OKD cri-o short-name 거부 대응
networkPolicy:
  kubernetesApiCidrs:
    - {{ k8s_api_cidr }}          # inventory 정본 (클러스터마다 다름)
  internal:
    - namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: greptimedb}}
      podSelector: {matchLabels: {{ dashboard_greptime_pod_labels | to_json }}}
      port: 4000
    - namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: quickwit}}
      podSelector: {matchLabels: {{ dashboard_quickwit_pod_labels | to_json }}}
      port: 7280
{% if cluster_distro == 'okd' %}
  dns:
    namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: openshift-dns}}
    podSelector: {matchLabels: {dns.operator.openshift.io/daemonset-dns: default}}
{% endif %}
```

</details>

주의: OKD apiserver egress(6443)와 DNS 5353은 chart values로 표현되지 않으므로,
deploy.sh의 `<release>-cluster-compat` NetworkPolicy에 해당하는 보완 정책 템플릿을
okd 분기에 추가해야 합니다(#26의 결정 그대로). 위 SCC j2와 같은 파일에 두면 됩니다.

## 4. templates/dashboard-external-secret.yaml.j2

reloader annotation이 핵심입니다 — Secret이 갱신되면 api Deployment가 자동 rollout됩니다
(chart의 `secretRevision` 수동 증가를 대체).

<details>
<summary>펼치기 — dashboard-external-secret.yaml.j2</summary>

```yaml
apiVersion: external-secrets.io/v1
kind: ExternalSecret
metadata:
  name: {{ dashboard_secret_name }}
  namespace: {{ dashboard_namespace }}
  annotations:
    reloader.stakater.com/match: "true"
spec:
  refreshInterval: 1h
  secretStoreRef: {kind: ClusterSecretStore, name: infisical}
  target:
    name: {{ dashboard_secret_name }}
  data:
{% for key in dashboard_secret_keys %}
    - secretKey: {{ key }}
      remoteRef: {key: "{{ dashboard_infisical_path }}/{{ key }}"}
{% endfor %}
```

</details>

api Deployment 쪽에는 chart values로 annotation을 넣을 수 없으므로, reloader가
Deployment를 굴리게 하려면 `reloader.stakater.com/auto: "true"` annotation을
post-render 또는 chart 개선(#별도 이슈: podAnnotations values 지원)으로 붙입니다.

## 5. uninstall/tasks/main.yaml

<details>
<summary>펼치기 — uninstall/tasks/main.yaml</summary>

```yaml
- name: Uninstall dashboard release
  kubernetes.core.helm:
    name: "{{ dashboard_release }}"
    release_namespace: "{{ dashboard_namespace }}"
    state: absent

- name: Remove dashboard auxiliary objects
  kubernetes.core.k8s:
    state: absent
    api_version: "{{ item.api }}"
    kind: "{{ item.kind }}"
    namespace: "{{ dashboard_namespace }}"
    name: "{{ item.name }}"
  loop:
    - {api: rbac.authorization.k8s.io/v1, kind: RoleBinding, name: "{{ dashboard_release }}-scc-nonroot-v2"}
    - {api: external-secrets.io/v1, kind: ExternalSecret, name: "{{ dashboard_secret_name }}"}
    - {api: route.openshift.io/v1, kind: Route, name: "{{ dashboard_release }}-ui"}
  when: item.name | length > 0
```

</details>

## 6. playbook·inventory 연결 예시

```yaml
# playbooks/<cluster>.install.yaml — 관측 스택(greptimedb/quickwit/opentelemetry) 뒤에
- name: Install observability dashboard
  hosts: all
  environment: "{{ k8s_env }}"
  tags: dashboard
  roles:
    - k8s-dashboard

# inventories/<cluster>.yaml
    dashboard_enabled: true
    dashboard_image_tag: v0.1.0
    dashboard_route_host: k8sdashboard.apps.okd.dtonic.io   # okd일 때
    k8s_api_cidr: 172.30.0.1/32
```

## 7. 남은 결정 (dhub2 리뷰에서 확정)

1. chart 반입 방식 — charts/ 미러에 tgz 고정 vs git ref. ADR 0013은 tgz를 권장.
2. `dashboard_quickwit_pod_labels` 기본값 — quickwit role의 실제 서비스 selector와
   대조해 확정 (`kubectl -n quickwit get svc quickwit-searcher -o jsonpath='{.spec.selector}'`).
3. reloader 연동을 위한 chart `podAnnotations` values 지원 (dashboard repo 별도 이슈).
4. OIDC 전환 시 `OIDC_CLIENT_SECRET`/`AUTH_SESSION_KEY`의 Infisical 경로 규약.
