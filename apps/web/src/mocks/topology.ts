/**
 * Pod Topology mock.
 * --------------------------------------------------------------------------
 * design-system의 `components/topology` preview와 같은 그래프를 API 형태로 제공합니다.
 * preview는 디자인 합의용 정적 문서이고, 여기서부터는 계약을 통과한 데이터입니다.
 *
 * 레이아웃(column/row)은 **서버가 정합니다.** 클라이언트가 매번 그래프 배치를 다시
 * 계산하면 갱신할 때마다 노드가 튀어 "어제 본 그림"과 달라집니다.
 */
import type {
  Protocol,
  RangeKey,
  TopologyEdge,
  TopologyNode,
  TopologyRoute,
  TrendSeries,
  UnhealthyEntity,
} from "@k8s-dashboard/contracts";
import { NOW_MS, rangeWindow } from "./data";
import { primaryPod } from "./drilldown";

function hash(seed: string): number {
  let h = 2166136261;
  for (let i = 0; i < seed.length; i++) {
    h ^= seed.charCodeAt(i);
    h = Math.imul(h, 16777619);
  }
  return ((h >>> 0) % 100000) / 100000;
}

/**
 * 그래프 노드. 이름과 UID는 drilldown mock의 **실제 Pod**에서 가져옵니다.
 * 여기서 이름을 새로 지어내면 노드 클릭 → Pod 상세가 404가 납니다.
 */
const NODE_DEF: Array<{ id: string; ns: string; workload: string; column: number; row: number }> = [
  { id: "gateway", ns: "ingress", workload: "gateway", column: 0, row: 1 },
  { id: "auth", ns: "payments", workload: "auth-svc", column: 1, row: 0 },
  { id: "payments", ns: "payments", workload: "payments-api", column: 1, row: 1 },
  { id: "redis", ns: "payments", workload: "redis-cache", column: 2, row: 0 },
  { id: "ledger", ns: "payments", workload: "ledger-worker", column: 2, row: 1 },
  { id: "batch", ns: "payments", workload: "batch-sync", column: 2, row: 2 },
  { id: "kafka", ns: "platform", workload: "kafka-broker", column: 3, row: 0 },
  { id: "postgres", ns: "platform", workload: "postgres-primary", column: 3, row: 2 },
];

let nodeCache: TopologyNode[] | null = null;

function nodes(): TopologyNode[] {
  if (nodeCache) return nodeCache;
  nodeCache = NODE_DEF.map((d) => {
    const { workload, pod, podCount } = primaryPod(d.ns, d.workload);
    return {
      id: d.id,
      ref: pod.ref,
      name: pod.name,
      namespace: d.ns,
      severity: workload.severity,
      column: d.column,
      row: d.row,
      podCount,
    };
  });
  return nodeCache;
}

/** 시간당 요청 수로 정의합니다. 범위가 바뀌면 곱해서 누적을 냅니다. */
const EDGE_DEF: Array<{
  from: string;
  to: string;
  severity: TopologyEdge["severity"];
  errorRate: number;
  routes: Array<[Protocol, string, number]>;
}> = [
  {
    from: "gateway", to: "payments", severity: "healthy", errorRate: 0.004,
    routes: [
      ["HTTP", "GET /api/v1/payments/{id}", 1420000],
      ["HTTP", "POST /api/v1/payments", 980000],
      ["HTTP", "GET /api/v1/payments/summary", 420000],
      ["HTTP", "POST /api/v1/payments/{id}/refund", 148000],
      ["HTTP", "GET /healthz", 63000],
    ],
  },
  {
    from: "gateway", to: "auth", severity: "healthy", errorRate: 0.002,
    routes: [["HTTP", "POST /api/v1/auth/token", 300000], ["HTTP", "GET /api/v1/auth/jwks", 124800]],
  },
  {
    from: "payments", to: "auth", severity: "healthy", errorRate: 0.006,
    routes: [
      ["gRPC", "auth.v1.AuthService/VerifyToken", 620000],
      ["gRPC", "auth.v1.AuthService/GetPrincipal", 118000],
    ],
  },
  {
    from: "auth", to: "payments", severity: "warning", errorRate: 0.061,
    routes: [["gRPC", "payments.v1.Ledger/Notify", 148000]],
  },
  {
    from: "payments", to: "redis", severity: "healthy", errorRate: 0.001,
    routes: [
      ["TCP", "redis:6379 GET", 3200000],
      ["TCP", "redis:6379 SET", 980000],
      ["TCP", "redis:6379 EVAL", 284000],
    ],
  },
  {
    from: "payments", to: "ledger", severity: "healthy", errorRate: 0.009,
    routes: [["HTTP", "POST /internal/ledger/entries", 890000], ["HTTP", "GET /internal/ledger/balance", 233200]],
  },
  {
    from: "ledger", to: "payments", severity: "warning", errorRate: 0.112,
    routes: [["HTTP", "POST /internal/payments/settle", 345600]],
  },
  {
    from: "payments", to: "postgres", severity: "healthy", errorRate: 0.002,
    routes: [
      ["TCP", "postgres:5432 SELECT", 1620000],
      ["TCP", "postgres:5432 INSERT", 468000],
      ["TCP", "postgres:5432 UPDATE", 86400],
    ],
  },
  {
    from: "ledger", to: "postgres", severity: "healthy", errorRate: 0.003,
    routes: [["TCP", "postgres:5432 SELECT", 1210000], ["TCP", "postgres:5432 INSERT", 510800]],
  },
  {
    from: "ledger", to: "kafka", severity: "healthy", errorRate: 0.001,
    routes: [["TCP", "produce ledger.entries", 795600]],
  },
  {
    from: "batch", to: "postgres", severity: "critical", errorRate: 0.973,
    routes: [["TCP", "postgres:5432 SELECT", 108000], ["TCP", "postgres:5432 COPY", 28800]],
  },
  {
    from: "batch", to: "kafka", severity: "critical", errorRate: 0.884,
    routes: [["UDP", "statsd metrics", 43200]],
  },
];

export function topologyGraph(range: RangeKey) {
  const { hours } = { hours: rangeHours(range) };
  const nodeList = nodes();
  const edges: TopologyEdge[] = EDGE_DEF.map((d) => {
    const routes: TopologyRoute[] = d.routes
      .map(([protocol, route, perHour]) => ({
        protocol,
        route,
        count: Math.round(perHour * hours),
        errorCount: Math.round(perHour * hours * d.errorRate),
      }))
      .sort((a, b) => b.count - a.count);
    return {
      id: `${d.from}->${d.to}`,
      from: d.from,
      to: d.to,
      severity: d.severity,
      totalCount: routes.reduce((s, r) => s + r.count, 0),
      errorRate: d.errorRate,
      protocols: [...new Set(routes.map((r) => r.protocol))],
      routes,
    };
  });
  return { nodes: nodeList, edges };
}

/** #3 회귀 시나리오: 수백 개 워크로드를 React Flow에서 실제로 렌더합니다. */
export function largeTopologyGraph(workloadCount = 500): { nodes: TopologyNode[]; edges: TopologyEdge[] } {
  const columns = Math.ceil(Math.sqrt(workloadCount));
  const nodeList: TopologyNode[] = Array.from({ length: workloadCount }, (_, i) => ({
    id: `large-${i}`,
    ref: {
      clusterId: "prod-seoul",
      namespace: `ns-${String(i % 10).padStart(2, "0")}`,
      workloadKind: "Deployment",
      workloadName: `workload-${String(i).padStart(3, "0")}`,
      workloadUid: `workload-uid-${i}`,
      podName: `workload-${String(i).padStart(3, "0")}-pod-0`,
      podUid: `pod-uid-${i}-0`,
    },
    name: `workload-${String(i).padStart(3, "0")}-pod-0`,
    namespace: `ns-${String(i % 10).padStart(2, "0")}`,
    severity: "healthy",
    column: 1 + (i % columns),
    row: Math.floor(i / columns),
    podCount: 2,
  }));
  const edges: TopologyEdge[] = nodeList.slice(1).map((node, i) => ({
    id: `${nodeList[i]!.id}->${node.id}`,
    from: nodeList[i]!.id,
    to: node.id,
    severity: "healthy",
    totalCount: 1_000,
    errorRate: 0,
    protocols: ["HTTP"],
    routes: [{ protocol: "HTTP", route: "GET /healthz", count: 1_000, errorCount: 0 }],
  }));
  return { nodes: nodeList, edges };
}

function rangeHours(range: RangeKey) {
  return range === "1h" ? 1 : range === "1d" ? 24 : range === "7d" ? 168 : range === "30d" ? 720 : 72;
}

const UNHEALTHY_DEF: Array<{ id: string; reason: string; restarts: number; forSeconds: number }> = [
  { id: "batch", reason: "CrashLoopBackOff", restarts: 14, forSeconds: 2460 },
  { id: "ledger", reason: "Readiness probe 실패 반복", restarts: 5, forSeconds: 7200 },
  { id: "payments", reason: "Replica 2/3 · p99 지연 상승", restarts: 2, forSeconds: 900 },
];

export function topologyUnhealthy(): UnhealthyEntity[] {
  return UNHEALTHY_DEF.map((d) => {
    const n = nodes().find((x) => x.id === d.id)!;
    return {
      ref: n.ref,
      name: n.name,
      kind: "Pod" as const,
      namespace: n.namespace,
      severity: n.severity,
      reason: d.reason,
      restarts: d.restarts,
      forSeconds: d.forSeconds,
    };
  });
}

/** 엣지 시계열 — 상위 3개 Route + 나머지는 "기타"로 접습니다. */
export function edgeSeries(edgeId: string, range: RangeKey): TrendSeries[] {
  const edge = topologyGraph(range).edges.find((e) => e.id === edgeId);
  if (!edge) return [];
  const { buckets, stepSeconds } = rangeWindow(range);
  const top = edge.routes.slice(0, 3);
  const rest = edge.routes.slice(3);
  const groups = [
    ...top.map((r) => ({ key: r.route, label: r.route.split("/").pop() || r.route, count: r.count })),
    ...(rest.length
      ? [{ key: "other", label: `기타 ${rest.length}개`, count: rest.reduce((s, r) => s + r.count, 0) }]
      : []),
  ];
  /* 집계 단위가 길수록 노이즈가 줄어듭니다. */
  const amp = 0.36 / Math.sqrt(stepSeconds / 60);
  return groups.map((g, i) => ({
    key: g.key,
    label: g.label,
    unit: "count" as const,
    points: Array.from({ length: buckets }, (_, b) => {
      const per = g.count / buckets;
      const wave = 1 + 0.22 * Math.sin((b / buckets) * Math.PI * 3 + i * 1.7);
      const jitter = 1 - amp / 2 + hash(`${edgeId}|${i}|${b}|${range}`) * amp;
      return { t: NOW_MS - (buckets - 1 - b) * stepSeconds * 1000, v: Math.max(0, per * wave * jitter) };
    }),
  }));
}
