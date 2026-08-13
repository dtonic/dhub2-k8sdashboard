/**
 * Alert mock (이슈 #17).
 * --------------------------------------------------------------------------
 * 자체 평가 엔진을 만들지 않습니다. Grafana Alerting / Alertmanager가 이미 판단한
 * 결과를 **공통 모델로 정규화**해 조회만 합니다. (README §2-7)
 *
 * 정규화 대상: severity, status, startsAt, endsAt, labels, annotations
 * Entity 매핑: label의 namespace / pod / workload를 Unified Entity Model로 옮깁니다.
 */
import type { AlertInstance, AlertSeverity, EntityRef, RangeKey } from "@k8s-dashboard/contracts";
import { NOW_MS } from "./data";
import { primaryPod } from "./drilldown";

/**
 * 중복 grouping 기준 — 화면에 그대로 노출합니다.
 * "왜 12건이 1건으로 보이는가"를 설명할 수 없으면 운영자는 화면을 믿지 않습니다.
 */
export const GROUPING_RULE =
  "alertname + namespace + workload 를 키로 묶습니다. Pod가 달라도 같은 Workload의 같은 규칙이면 한 건으로 봅니다. " +
  "cluster 라벨은 이미 Scope로 고정되어 키에 넣지 않습니다.";

const min = (m: number) => m * 60 * 1000;

type Def = {
  name: string;
  severity: AlertSeverity;
  startedMinAgo: number;
  resolvedMinAgo?: number;
  labels: Record<string, string>;
  annotations: Record<string, string>;
  entity?: EntityRef;
  entityName?: string;
  groupSize: number;
  source: "grafana" | "alertmanager";
};

const DEFS: Def[] = [
  {
    name: "KubePodCrashLooping",
    severity: "critical",
    startedMinAgo: 41,
    labels: { alertname: "KubePodCrashLooping", namespace: "payments", workload: "batch-sync", severity: "critical" },
    annotations: {
      summary: "Pod가 반복 재시작 중입니다",
      description: "batch-sync-qq81z 컨테이너 app이 최근 41분간 14회 재시작했습니다.",
      runbook_url: "docs/runbooks/crashloop.md",
    },
    /* entity는 실제 Pod을 가리켜야 deep link가 동작합니다. */
    entity: undefined,
    entityName: undefined,
    groupSize: 1,
    source: "grafana",
  },
  {
    name: "KubeNodeNotReady",
    severity: "critical",
    startedMinAgo: 9,
    labels: { alertname: "KubeNodeNotReady", node: "ip-10-0-31-207", severity: "critical" },
    annotations: { summary: "Node가 NotReady 상태입니다", description: "kubelet이 9분간 응답하지 않습니다." },
    entityName: "ip-10-0-31-207",
    groupSize: 1,
    source: "alertmanager",
  },
  {
    name: "HighRequestLatency",
    severity: "warning",
    startedMinAgo: 22,
    labels: { alertname: "HighRequestLatency", namespace: "payments", workload: "payments-api", severity: "warning" },
    annotations: {
      summary: "p99 지연이 SLO를 초과했습니다",
      description: "p99 = 812ms (SLO 500ms). 상위 원인은 auth-svc 호출 지연입니다.",
    },
    entity: {
      clusterId: "prod-seoul",
      namespace: "payments",
      workloadKind: "Deployment",
      workloadName: "payments-api",
    },
    entityName: "payments-api",
    /* 3개 Pod에서 각각 발생했지만 같은 Workload의 같은 규칙이라 한 건으로 묶입니다. */
    groupSize: 3,
    source: "grafana",
  },
  {
    name: "KubeMemoryOvercommit",
    severity: "warning",
    startedMinAgo: 180,
    labels: { alertname: "KubeMemoryOvercommit", severity: "warning" },
    annotations: { summary: "클러스터 메모리가 초과 할당되었습니다", description: "Request 합계가 가용 메모리의 112%입니다." },
    groupSize: 1,
    source: "alertmanager",
  },
  {
    name: "TargetDown",
    severity: "warning",
    startedMinAgo: 12,
    labels: { alertname: "TargetDown", namespace: "search", workload: "indexer", severity: "warning" },
    annotations: { summary: "스크레이프 대상이 응답하지 않습니다", description: "indexer-1 메트릭 엔드포인트 3/3 실패." },
    entity: { clusterId: "prod-seoul", namespace: "search", workloadKind: "StatefulSet", workloadName: "indexer" },
    entityName: "indexer",
    groupSize: 2,
    source: "alertmanager",
  },
  {
    name: "CertificateExpiringSoon",
    severity: "info",
    startedMinAgo: 1440,
    labels: { alertname: "CertificateExpiringSoon", namespace: "ingress", severity: "info" },
    annotations: { summary: "인증서 만료가 임박했습니다", description: "*.payments.internal 인증서가 12일 후 만료됩니다." },
    entity: { clusterId: "prod-seoul", namespace: "ingress" },
    entityName: "ingress",
    groupSize: 1,
    source: "grafana",
  },
  /* ── 해소된 알림 ── */
  {
    name: "KubePodNotReady",
    severity: "critical",
    startedMinAgo: 320,
    resolvedMinAgo: 268,
    labels: { alertname: "KubePodNotReady", namespace: "payments", workload: "ledger-worker", severity: "critical" },
    annotations: { summary: "Pod가 Ready 상태가 되지 못했습니다", description: "롤아웃 이후 52분간 지속되었습니다." },
    entity: {
      clusterId: "prod-seoul",
      namespace: "payments",
      workloadKind: "StatefulSet",
      workloadName: "ledger-worker",
    },
    entityName: "ledger-worker",
    groupSize: 2,
    source: "grafana",
  },
  {
    name: "HighRequestLatency",
    severity: "warning",
    startedMinAgo: 700,
    resolvedMinAgo: 640,
    labels: { alertname: "HighRequestLatency", namespace: "search", workload: "indexer", severity: "warning" },
    annotations: { summary: "p99 지연이 SLO를 초과했습니다", description: "인덱스 재구축 중 발생, 완료 후 해소." },
    entity: { clusterId: "prod-seoul", namespace: "search", workloadKind: "StatefulSet", workloadName: "indexer" },
    entityName: "indexer",
    groupSize: 1,
    source: "grafana",
  },
  {
    name: "DiskPressure",
    severity: "warning",
    startedMinAgo: 2600,
    resolvedMinAgo: 2540,
    labels: { alertname: "DiskPressure", node: "ip-10-0-14-88", severity: "warning" },
    annotations: { summary: "노드 디스크 여유가 부족합니다", description: "이미지 GC 후 해소되었습니다." },
    entityName: "ip-10-0-14-88",
    groupSize: 1,
    source: "alertmanager",
  },
];

const fingerprint = (d: Def) =>
  `${d.labels.alertname}|${d.labels.namespace ?? "-"}|${d.labels.workload ?? d.labels.node ?? "-"}|${d.startedMinAgo}`;

const groupKeyOf = (d: Def) =>
  `alertname=${d.labels.alertname} namespace=${d.labels.namespace ?? "-"} workload=${d.labels.workload ?? "-"}`;

function toInstance(d: Def): AlertInstance {
  /* label의 namespace/workload를 Unified Entity Model로 옮깁니다. (이슈 #17 작업 범위)
     Pod 단위 알림은 해당 Workload의 대표 Pod로 연결합니다. */
  let entity = d.entity;
  let entityName = d.entityName;
  if (!entity && d.labels.namespace && d.labels.workload) {
    try {
      const { workload, pod } = primaryPod(d.labels.namespace, d.labels.workload);
      entity =
        d.labels.alertname === "KubePodCrashLooping"
          ? pod.ref
          : {
              clusterId: "prod-seoul",
              namespace: workload.namespace,
              workloadKind: workload.kind,
              workloadName: workload.name,
              workloadUid: workload.ref.workloadUid,
            };
      entityName = d.labels.alertname === "KubePodCrashLooping" ? pod.name : workload.name;
    } catch {
      /* mock에 없는 워크로드면 매핑 없이 둡니다. 화면이 "매핑 없음"을 표시합니다. */
    }
  }
  return {
    id: fingerprint(d),
    name: d.name,
    severity: d.severity,
    status: d.resolvedMinAgo === undefined ? "firing" : "resolved",
    startsAt: new Date(NOW_MS - min(d.startedMinAgo)).toISOString(),
    endsAt: d.resolvedMinAgo === undefined ? undefined : new Date(NOW_MS - min(d.resolvedMinAgo)).toISOString(),
    labels: d.labels,
    annotations: d.annotations,
    entity,
    entityName,
    /* 원본 시스템 상세. MVP에서 Rule 편집·Silence·Routing 변경은 여기서 하지 않습니다. */
    sourceUrl:
      d.source === "grafana"
        ? `grafana/alerting/list?search=${encodeURIComponent(d.name)}`
        : `alertmanager/#/alerts?filter=${encodeURIComponent(`alertname="${d.name}"`)}`,
    source: d.source,
    groupSize: d.groupSize,
    groupKey: groupKeyOf(d),
  };
}

const rangeMinutes = (range: RangeKey) =>
  range === "1h" ? 60 : range === "1d" ? 1440 : range === "7d" ? 10080 : range === "30d" ? 43200 : 4320;

export function alertList(range: RangeKey, ns: string) {
  const within = rangeMinutes(range);
  const all = DEFS.filter((d) => !ns || d.labels.namespace === ns).map((d) => ({ d, a: toInstance(d) }));

  /* firing은 범위와 무관하게 "지금 진행 중"입니다. resolved만 범위로 자릅니다. */
  const firing = all.filter(({ a }) => a.status === "firing").map(({ a }) => a);
  const resolved = all
    .filter(({ d, a }) => a.status === "resolved" && (d.resolvedMinAgo ?? 0) <= within)
    .map(({ a }) => a);

  const counts = {
    critical: { firing: 0, resolved: 0 },
    warning: { firing: 0, resolved: 0 },
    info: { firing: 0, resolved: 0 },
  } as Record<AlertSeverity, { firing: number; resolved: number }>;
  for (const a of firing) counts[a.severity].firing += 1;
  for (const a of resolved) counts[a.severity].resolved += 1;

  return { firing, resolved, counts };
}
