/**
 * Mock 데이터 생성기.
 * --------------------------------------------------------------------------
 * API가 아직 없으므로 UI를 독립 실행하기 위한 고정 데이터입니다. (이슈 #13 완료 기준)
 * 난수는 결정적입니다 — 스크린샷 리뷰와 시각 회귀 비교가 흔들리지 않아야 합니다.
 */
import {
  type AlertSummary,
  type ClusterEvent,
  type ClusterOverviewResponse,
  type NodeHealth,
  type PodHealth,
  type RangeKey,
  type Section,
  type TopologySummary,
  type TrendPanel,
  type UnhealthyEntity,
  type WorkloadHealth,
  STEP_SECONDS,
} from "@k8s-dashboard/contracts";

/** 고정 기준 시각 — 2026-08-13 13:00 KST */
export const NOW_MS = Date.UTC(2026, 7, 13, 4, 0, 0);

function hash(seed: string): number {
  let h = 2166136261;
  for (let i = 0; i < seed.length; i++) {
    h ^= seed.charCodeAt(i);
    h = Math.imul(h, 16777619);
  }
  return ((h >>> 0) % 100000) / 100000;
}

export function rangeWindow(key: RangeKey) {
  const stepSeconds = key === "custom" ? 900 : STEP_SECONDS[key];
  const spanSeconds =
    key === "1h" ? 3600 : key === "1d" ? 86400 : key === "7d" ? 604800 : key === "30d" ? 2592000 : 259200;
  const buckets = Math.round(spanSeconds / stepSeconds);
  return {
    stepSeconds,
    buckets,
    from: new Date(NOW_MS - spanSeconds * 1000).toISOString(),
    to: new Date(NOW_MS).toISOString(),
  };
}

function series(seed: string, buckets: number, stepSeconds: number, base: number, amplitude: number) {
  /* 버킷이 길수록 집계로 노이즈가 줄어듭니다. */
  const noiseAmp = amplitude / Math.sqrt(stepSeconds / 60);
  const points = [];
  for (let i = 0; i < buckets; i++) {
    const wave = Math.sin((i / buckets) * Math.PI * 2.4) * amplitude * 0.6;
    const jitter = (hash(`${seed}|${i}|${buckets}`) - 0.5) * 2 * noiseAmp;
    points.push({ t: NOW_MS - (buckets - 1 - i) * stepSeconds * 1000, v: Math.max(0, base + wave + jitter) });
  }
  return points;
}

const NODES: NodeHealth = { total: 18, ready: 16, notReady: 1, pressure: 1, unschedulable: 0 };
const PODS: PodHealth = {
  total: 412,
  running: 389,
  pending: 7,
  failed: 3,
  crashLoopBackOff: 2,
  imagePullBackOff: 1,
  restarts: 41,
};
const WORKLOADS: WorkloadHealth = { total: 96, available: 89, replicaMismatch: 5, rolloutStalled: 2 };

const UNHEALTHY: UnhealthyEntity[] = [
  {
    ref: { clusterId: "prod-seoul", namespace: "payments", podName: "batch-sync-qq81z", podUid: "5c1f-…-9a2e" },
    name: "batch-sync-qq81z",
    kind: "Pod",
    namespace: "payments",
    severity: "critical",
    reason: "CrashLoopBackOff",
    restarts: 14,
    forSeconds: 2460,
  },
  {
    ref: { clusterId: "prod-seoul", namespace: "search", podName: "indexer-7c4b2", podUid: "a71b-…-33cd" },
    name: "indexer-7c4b2",
    kind: "Pod",
    namespace: "search",
    severity: "critical",
    reason: "CrashLoopBackOff",
    restarts: 9,
    forSeconds: 1080,
  },
  {
    ref: {
      clusterId: "prod-seoul",
      namespace: "payments",
      workloadKind: "StatefulSet",
      workloadName: "ledger-worker",
      workloadUid: "b0d2-…-71fa",
    },
    name: "ledger-worker",
    kind: "Workload",
    namespace: "payments",
    severity: "degraded",
    reason: "Readiness probe 실패 반복",
    restarts: 5,
    forSeconds: 7200,
  },
  {
    ref: {
      clusterId: "prod-seoul",
      namespace: "payments",
      workloadKind: "Deployment",
      workloadName: "payments-api",
      workloadUid: "3fe8-…-1c04",
    },
    name: "payments-api",
    kind: "Workload",
    namespace: "payments",
    severity: "warning",
    reason: "Replica 2/3 · p99 지연 상승",
    restarts: 2,
    forSeconds: 900,
  },
  {
    ref: { clusterId: "prod-seoul", namespace: "media", podName: "transcoder-x91kd", podUid: "cc41-…-77b1" },
    name: "transcoder-x91kd",
    kind: "Pod",
    namespace: "media",
    severity: "warning",
    reason: "ImagePullBackOff",
    restarts: 0,
    forSeconds: 420,
  },
  {
    ref: { clusterId: "prod-seoul", podName: "ip-10-0-31-207" },
    name: "ip-10-0-31-207",
    kind: "Node",
    namespace: "—",
    severity: "critical",
    reason: "NotReady · kubelet 응답 없음",
    restarts: 0,
    forSeconds: 540,
  },
  {
    ref: { clusterId: "prod-seoul", podName: "ip-10-0-14-88" },
    name: "ip-10-0-14-88",
    kind: "Node",
    namespace: "—",
    severity: "warning",
    reason: "MemoryPressure",
    restarts: 0,
    forSeconds: 3300,
  },
];

export const EVENTS: ClusterEvent[] = [
  {
    id: "ev-1",
    type: "Warning",
    reason: "BackOff",
    message: "Back-off restarting failed container app in pod batch-sync-qq81z",
    involved: { clusterId: "prod-seoul", namespace: "payments", podName: "batch-sync-qq81z" },
    involvedName: "batch-sync-qq81z",
    namespace: "payments",
    count: 14,
    lastSeen: new Date(NOW_MS - 42 * 1000).toISOString(),
  },
  {
    id: "ev-2",
    type: "Warning",
    reason: "NodeNotReady",
    message: "Node ip-10-0-31-207 status is now: NodeNotReady",
    involved: { clusterId: "prod-seoul", podName: "ip-10-0-31-207" },
    involvedName: "ip-10-0-31-207",
    namespace: "—",
    count: 1,
    lastSeen: new Date(NOW_MS - 9 * 60 * 1000).toISOString(),
  },
  {
    id: "ev-3",
    type: "Warning",
    reason: "FailedScheduling",
    message: "0/18 nodes are available: 3 Insufficient memory, 15 node(s) didn't match affinity",
    involved: { clusterId: "prod-seoul", namespace: "search", podName: "reranker-0" },
    involvedName: "reranker-0",
    namespace: "search",
    count: 6,
    lastSeen: new Date(NOW_MS - 3 * 60 * 1000).toISOString(),
  },
  {
    id: "ev-4",
    type: "Warning",
    reason: "Unhealthy",
    message: "Readiness probe failed: HTTP probe failed with statuscode: 500",
    involved: { clusterId: "prod-seoul", namespace: "payments", podName: "ledger-worker-9fkt2" },
    involvedName: "ledger-worker-9fkt2",
    namespace: "payments",
    count: 23,
    lastSeen: new Date(NOW_MS - 70 * 1000).toISOString(),
  },
  {
    id: "ev-5",
    type: "Warning",
    reason: "Failed",
    message: 'Failed to pull image "registry.internal/transcoder:2.9.1": not found',
    involved: { clusterId: "prod-seoul", namespace: "media", podName: "transcoder-x91kd" },
    involvedName: "transcoder-x91kd",
    namespace: "media",
    count: 4,
    lastSeen: new Date(NOW_MS - 6 * 60 * 1000).toISOString(),
  },
  {
    id: "ev-6",
    type: "Normal",
    reason: "ScalingReplicaSet",
    message: "Scaled up replica set payments-api-7d9c4f8b6c to 3",
    involved: {
      clusterId: "prod-seoul",
      namespace: "payments",
      workloadKind: "Deployment",
      workloadName: "payments-api",
    },
    involvedName: "payments-api",
    namespace: "payments",
    count: 1,
    lastSeen: new Date(NOW_MS - 14 * 60 * 1000).toISOString(),
  },
];

const ALERTS: AlertSummary = {
  bySeverity: { critical: 2, warning: 5, info: 3 },
  top: [
    {
      id: "al-1",
      name: "KubePodCrashLooping",
      severity: "critical",
      namespace: "payments",
      activeSince: new Date(NOW_MS - 41 * 60 * 1000).toISOString(),
    },
    {
      id: "al-2",
      name: "KubeNodeNotReady",
      severity: "critical",
      namespace: "—",
      activeSince: new Date(NOW_MS - 9 * 60 * 1000).toISOString(),
    },
    {
      id: "al-3",
      name: "HighRequestLatency",
      severity: "warning",
      namespace: "payments",
      activeSince: new Date(NOW_MS - 22 * 60 * 1000).toISOString(),
    },
    {
      id: "al-4",
      name: "KubeMemoryOvercommit",
      severity: "warning",
      namespace: "—",
      activeSince: new Date(NOW_MS - 3 * 3600 * 1000).toISOString(),
    },
  ],
};

const TOPOLOGY: TopologySummary = {
  pods: 412,
  edges: 118,
  problemEdges: [
    {
      from: "batch-sync-qq81z",
      to: "postgres-primary-0",
      protocol: "TCP",
      requestsPerSecond: 38,
      errorRate: 0.97,
      severity: "critical",
    },
    {
      from: "ledger-worker-9fkt2",
      to: "payments-api-x2mkq",
      protocol: "HTTP",
      requestsPerSecond: 96,
      errorRate: 0.11,
      severity: "warning",
    },
    {
      from: "auth-svc-8b41d",
      to: "payments-api-x2mkq",
      protocol: "gRPC",
      requestsPerSecond: 41,
      errorRate: 0.06,
      severity: "warning",
    },
  ],
};

function trends(key: RangeKey): TrendPanel[] {
  const { buckets, stepSeconds } = rangeWindow(key);
  return [
    {
      id: "cpu",
      title: "CPU 사용률",
      stepSeconds,
      series: [
        { key: "used", label: "사용량", unit: "percent", points: series("cpu-used", buckets, stepSeconds, 58, 12) },
        { key: "requested", label: "Request", unit: "percent", points: series("cpu-req", buckets, stepSeconds, 74, 8) },
      ],
    },
    {
      id: "memory",
      title: "Memory Working Set",
      stepSeconds,
      series: [
        { key: "used", label: "사용량", unit: "percent", points: series("mem-used", buckets, stepSeconds, 63, 9) },
        { key: "requested", label: "Request", unit: "percent", points: series("mem-req", buckets, stepSeconds, 81, 6) },
      ],
    },
    {
      id: "io",
      title: "Disk I/O",
      stepSeconds,
      series: [
        { key: "read", label: "읽기", unit: "bytes_per_sec", points: series("io-read", buckets, stepSeconds, 240, 55) },
        { key: "write", label: "쓰기", unit: "bytes_per_sec", points: series("io-write", buckets, stepSeconds, 120, 35) },
      ],
    },
    {
      id: "network",
      title: "Network 처리량",
      stepSeconds,
      series: [
        { key: "rx", label: "수신", unit: "bytes_per_sec", points: series("net-rx", buckets, stepSeconds, 420, 90) },
        { key: "tx", label: "송신", unit: "bytes_per_sec", points: series("net-tx", buckets, stepSeconds, 310, 70) },
      ],
    },
    {
      id: "restarts",
      title: "Container 재시작",
      stepSeconds,
      series: [
        { key: "restarts", label: "재시작", unit: "count", points: series("restart", buckets, stepSeconds, 1.4, 1.1) },
      ],
    },
  ];
}

export type Scenario = "default" | "degraded" | "forbidden" | "empty";

const ok = <T,>(data: T): Section<T> => ({ status: "ok", data, observedAt: new Date(NOW_MS).toISOString() });

export function buildOverview(key: RangeKey, scenario: Scenario): ClusterOverviewResponse {
  const w = rangeWindow(key);
  const base: ClusterOverviewResponse = {
    clusterId: "prod-seoul",
    clusterName: "prod-seoul",
    appliedScope: { clusterId: "prod-seoul", namespaces: "all" },
    range: { key, from: w.from, to: w.to, stepSeconds: w.stepSeconds },
    generatedAt: new Date(NOW_MS).toISOString(),
    nodes: ok(NODES),
    pods: ok(PODS),
    workloads: ok(WORKLOADS),
    trends: ok(trends(key)),
    unhealthy: ok(UNHEALTHY),
    events: ok(EVENTS),
    alerts: ok(ALERTS),
    topology: ok(TOPOLOGY),
  };

  if (scenario === "degraded") {
    /* Quickwit과 Alertmanager가 죽어도 나머지 화면은 계속 동작해야 합니다. */
    base.events = {
      status: "degraded",
      source: "quickwit",
      reason: "Quickwit 응답 없음 (timeout 5s)",
      observedAt: new Date(NOW_MS - 12 * 60 * 1000).toISOString(),
      data: EVENTS.slice(0, 3),
    };
    base.alerts = {
      status: "degraded",
      source: "alertmanager",
      reason: "Alertmanager 연결 실패",
    };
    base.trends = {
      status: "degraded",
      source: "greptimedb",
      reason: "GreptimeDB 부분 응답 · Network 패널 누락",
      observedAt: new Date(NOW_MS - 4 * 60 * 1000).toISOString(),
      data: trends(key).filter((t) => t.id !== "network"),
    };
  }

  if (scenario === "forbidden") {
    /* Scope 밖 — "데이터 없음"과 구분되어야 합니다. */
    base.appliedScope = { clusterId: "prod-seoul", namespaces: ["payments"] };
    base.events = { status: "forbidden", reason: "cluster.viewer 권한이 필요합니다" };
    base.alerts = { status: "forbidden", reason: "alert.viewer 권한이 필요합니다" };
    base.unhealthy = {
      status: "ok",
      data: UNHEALTHY.filter((u) => u.namespace === "payments"),
      observedAt: new Date(NOW_MS).toISOString(),
    };
    base.topology = { status: "forbidden", reason: "cluster.viewer 권한이 필요합니다" };
  }

  if (scenario === "empty") {
    base.unhealthy = { status: "empty", observedAt: new Date(NOW_MS).toISOString(), data: [] };
    base.events = { status: "empty", observedAt: new Date(NOW_MS).toISOString(), data: [] };
    base.alerts = {
      status: "ok",
      data: { bySeverity: { critical: 0, warning: 0, info: 0 }, top: [] },
      observedAt: new Date(NOW_MS).toISOString(),
    };
    base.topology = {
      status: "ok",
      data: { ...TOPOLOGY, problemEdges: [] },
      observedAt: new Date(NOW_MS).toISOString(),
    };
    base.pods = ok({ ...PODS, pending: 0, failed: 0, crashLoopBackOff: 0, imagePullBackOff: 0, restarts: 0 });
    base.nodes = ok({ ...NODES, ready: 18, notReady: 0, pressure: 0 });
    base.workloads = ok({ ...WORKLOADS, available: 96, replicaMismatch: 0, rolloutStalled: 0 });
  }

  return base;
}

export const SCOPE = {
  clusters: [
    { id: "prod-seoul", name: "prod-seoul", namespaces: "all" as const, accessible: true },
    { id: "prod-tokyo", name: "prod-tokyo", namespaces: ["payments", "search"], accessible: true },
    { id: "stage", name: "stage", namespaces: "all" as const, accessible: true },
    { id: "prod-frankfurt", name: "prod-frankfurt", namespaces: [], accessible: false },
  ],
  canManageWorkloads: true,
};

export const NAMESPACES = ["payments", "search", "media", "platform", "ingress", "observability"];
