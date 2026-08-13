/**
 * Drill-down mock 데이터 (이슈 #15).
 * --------------------------------------------------------------------------
 * Namespace → Workload → Pod → Container를 실제 Kubernetes 관계에 맞게 만듭니다.
 *
 * 일부러 재현해 둔 것들
 * - `payments`는 Workload 240개. 가상 스크롤이 실제로 필요한 규모여야 검증이 됩니다.
 * - `payments-api`는 롤아웃 중이라 ReplicaSet이 두 개 공존합니다(구/신).
 * - `batch-sync`는 이름이 같지만 UID가 다른 Pod 인스턴스가 있습니다. 재생성 구분용입니다.
 * - `prod-tokyo`는 payments/search만 접근 가능합니다. 그 외 Namespace 직접 접근은 거절됩니다.
 */
import type {
  ContainerStatus,
  IssueReason,
  NamespaceSummary,
  OwnerRef,
  PodSummary,
  RangeKey,
  ResourceUsage,
  Severity,
  TrendPanel,
  WorkloadKind,
  WorkloadSummary,
} from "@k8s-dashboard/contracts";
import { NOW_MS, rangeWindow } from "./data";

function hash(seed: string): number {
  let h = 2166136261;
  for (let i = 0; i < seed.length; i++) {
    h ^= seed.charCodeAt(i);
    h = Math.imul(h, 16777619);
  }
  return ((h >>> 0) % 100000) / 100000;
}

const pick = <T,>(seed: string, arr: readonly T[]): T => arr[Math.floor(hash(seed) * arr.length) % arr.length]!;
const int = (seed: string, min: number, max: number) => min + Math.floor(hash(seed) * (max - min + 1));

/** 짧은 hex 접미사. 실제 Pod 이름 모양을 흉내 냅니다. */
const suffix = (seed: string, len = 5) =>
  Math.floor(hash(seed) * 0xfffff)
    .toString(16)
    .padStart(len, "0")
    .slice(0, len);

const uid = (seed: string) =>
  `${suffix(seed + "a", 8)}-${suffix(seed + "b", 4)}-${suffix(seed + "c", 4)}-${suffix(seed + "d", 12)}`;

function usage(seed: string, scale = 1): ResourceUsage {
  const cpuRequestMilli = int(seed + "cr", 100, 800) * scale;
  const cpuLimitMilli = hash(seed + "cl") > 0.25 ? cpuRequestMilli * 2 : null;
  const cpuMilli = Math.round(cpuRequestMilli * (0.28 + hash(seed + "cu") * 0.9));
  const memoryRequestMib = int(seed + "mr", 128, 2048) * scale;
  const memoryLimitMib = hash(seed + "ml") > 0.2 ? memoryRequestMib * 2 : null;
  const memoryMib = Math.round(memoryRequestMib * (0.34 + hash(seed + "mu") * 0.85));
  return {
    cpuMilli,
    cpuRequestMilli,
    cpuLimitMilli,
    memoryMib,
    memoryRequestMib,
    memoryLimitMib,
    cpuVsRequest: cpuMilli / cpuRequestMilli,
    cpuVsLimit: cpuLimitMilli ? cpuMilli / cpuLimitMilli : null,
    memoryVsRequest: memoryMib / memoryRequestMib,
    memoryVsLimit: memoryLimitMib ? memoryMib / memoryLimitMib : null,
  };
}

/* ── Namespace 정의 ──────────────────────────────────────────────────────── */

const NS_DEF = [
  { name: "payments", workloads: 240 },
  { name: "search", workloads: 46 },
  { name: "media", workloads: 28 },
  { name: "platform", workloads: 34 },
  { name: "ingress", workloads: 9 },
  { name: "observability", workloads: 17 },
] as const;

export const NAMESPACE_NAMES = NS_DEF.map((n) => n.name);

const KINDS: WorkloadKind[] = ["Deployment", "Deployment", "Deployment", "StatefulSet", "DaemonSet", "CronJob"];

const SERVICE_WORDS = [
  "api", "worker", "gateway", "indexer", "sync", "router", "cache", "auth", "ledger",
  "billing", "notify", "report", "cleaner", "exporter", "scheduler", "webhook", "proxy",
] as const;

/** 손으로 고정한 대표 워크로드 — 화면 검증에 쓰는 시나리오들 */
const PINNED: Record<string, Array<Partial<WorkloadSummary> & { name: string; kind: WorkloadKind }>> = {
  payments: [
    {
      name: "payments-api",
      kind: "Deployment",
      severity: "warning",
      replicas: { desired: 3, ready: 2, available: 2, updated: 2 },
      rollout: { status: "Progressing", message: "새 ReplicaSet이 3개 중 2개만 준비됨" },
      restarts: 2,
      issues: ["ReplicaMismatch", "ProbeFailed"],
      images: ["registry.internal/payments-api:2.14.3"],
    },
    {
      name: "ledger-worker",
      kind: "StatefulSet",
      severity: "degraded",
      replicas: { desired: 2, ready: 0, available: 0, updated: 2 },
      rollout: { status: "Stalled", message: "Readiness probe 실패가 10분 이상 지속됨" },
      restarts: 5,
      issues: ["ProbeFailed", "Restarting"],
      images: ["registry.internal/ledger-worker:1.8.0"],
    },
    {
      name: "batch-sync",
      kind: "CronJob",
      severity: "critical",
      replicas: { desired: 1, ready: 0, available: 0, updated: 1 },
      rollout: { status: "Stalled", message: "컨테이너가 반복 종료됨 (exit 2)" },
      restarts: 14,
      issues: ["CrashLoopBackOff"],
      images: ["registry.internal/batch-sync:0.9.4"],
    },
    {
      name: "auth-svc",
      kind: "Deployment",
      severity: "healthy",
      replicas: { desired: 4, ready: 4, available: 4, updated: 4 },
      rollout: { status: "Complete" },
      restarts: 0,
      issues: [],
      images: ["registry.internal/auth-svc:3.2.1"],
    },
  ],
  media: [
    {
      name: "transcoder",
      kind: "Deployment",
      severity: "warning",
      replicas: { desired: 6, ready: 5, available: 5, updated: 5 },
      rollout: { status: "Progressing", message: "이미지 태그를 찾을 수 없음" },
      restarts: 0,
      issues: ["ImagePullBackOff", "ReplicaMismatch"],
      images: ["registry.internal/transcoder:2.9.1"],
    },
  ],
  search: [
    {
      name: "indexer",
      kind: "StatefulSet",
      severity: "critical",
      replicas: { desired: 3, ready: 1, available: 1, updated: 3 },
      rollout: { status: "Stalled", message: "OOMKilled 반복" },
      restarts: 9,
      issues: ["CrashLoopBackOff", "OOMKilled"],
      images: ["registry.internal/search-indexer:4.1.0"],
    },
  ],
};

function generatedWorkload(ns: string, i: number): WorkloadSummary {
  const seed = `${ns}/${i}`;
  const kind = pick(seed + "k", KINDS);
  const name = `${pick(seed + "n1", SERVICE_WORDS)}-${pick(seed + "n2", SERVICE_WORDS)}-${suffix(seed + "n3", 3)}`;
  const desired = kind === "DaemonSet" ? 18 : int(seed + "d", 1, 8);
  const roll = hash(seed + "r");
  const ready = roll > 0.88 ? Math.max(0, desired - int(seed + "m", 1, 2)) : desired;
  const restarts = roll > 0.94 ? int(seed + "rs", 1, 6) : 0;
  const issues: IssueReason[] = [];
  if (ready < desired) issues.push("ReplicaMismatch");
  if (restarts > 0) issues.push("Restarting");
  const severity: Severity = ready === 0 ? "critical" : issues.length ? "warning" : "healthy";
  return {
    ref: { clusterId: "prod-seoul", namespace: ns, workloadKind: kind, workloadName: name, workloadUid: uid(seed) },
    name,
    kind,
    namespace: ns,
    severity,
    replicas: { desired, ready, available: ready, updated: desired },
    rollout: ready < desired ? { status: "Progressing" } : { status: "Complete" },
    restarts,
    usage: usage(seed),
    images: [`registry.internal/${name.split("-")[0]}:${int(seed + "v", 1, 9)}.${int(seed + "v2", 0, 20)}.0`],
    issues,
    ageSeconds: int(seed + "age", 3600, 60 * 86400),
  };
}

const workloadCache = new Map<string, WorkloadSummary[]>();

export function workloadsOf(ns: string): WorkloadSummary[] {
  const cached = workloadCache.get(ns);
  if (cached) return cached;
  const def = NS_DEF.find((d) => d.name === ns);
  const pinned = (PINNED[ns] ?? []).map((p) => {
    const base = generatedWorkload(ns, -1);
    return {
      ...base,
      ...p,
      ref: {
        clusterId: "prod-seoul",
        namespace: ns,
        workloadKind: p.kind,
        workloadName: p.name,
        workloadUid: uid(`${ns}/${p.name}`),
      },
      namespace: ns,
      usage: usage(`${ns}/${p.name}`, 2),
      ageSeconds: int(`${ns}/${p.name}age`, 3600, 30 * 86400),
    } as WorkloadSummary;
  });
  const rest = Array.from({ length: Math.max(0, (def?.workloads ?? 12) - pinned.length) }, (_, i) =>
    generatedWorkload(ns, i),
  );
  const all = [...pinned, ...rest];
  workloadCache.set(ns, all);
  return all;
}

/* ── Pod ─────────────────────────────────────────────────────────────────── */

/**
 * Deployment는 ReplicaSet을 거쳐 Pod를 소유합니다. 롤아웃 중이면 두 세대가 공존하므로
 * 체인을 정확히 만들어 화면에서 그대로 보여줄 수 있게 합니다. (이슈 #15 완료 기준)
 */
export function ownerChainOf(w: WorkloadSummary): OwnerRef[] {
  if (w.kind !== "Deployment") return [];
  const rolling = w.rollout.status === "Progressing" && w.replicas.updated < w.replicas.desired;
  const current: OwnerRef = {
    kind: "ReplicaSet",
    name: `${w.name}-${suffix(w.name + "rs-new", 10)}`,
    uid: uid(w.name + "rs-new"),
    current: true,
    pods: rolling ? w.replicas.updated : w.replicas.ready,
    revision: `${int(w.name + "rev", 4, 40)}`,
  };
  if (!rolling) return [current];
  const previous: OwnerRef = {
    kind: "ReplicaSet",
    name: `${w.name}-${suffix(w.name + "rs-old", 10)}`,
    uid: uid(w.name + "rs-old"),
    current: false,
    pods: Math.max(0, w.replicas.ready - w.replicas.updated),
    revision: `${int(w.name + "rev", 4, 40) - 1}`,
  };
  return [current, previous];
}

function podIssues(w: WorkloadSummary, healthy: boolean): IssueReason[] {
  if (healthy) return [];
  return w.issues.filter((i) => i !== "ReplicaMismatch" && i !== "RolloutStalled");
}

export function podsOf(w: WorkloadSummary): PodSummary[] {
  const chain = ownerChainOf(w);
  const owner = chain.find((c) => c.current) ?? chain[0];
  const total = Math.max(w.replicas.desired, w.replicas.ready);
  const pods: PodSummary[] = [];

  for (let i = 0; i < total; i++) {
    const seed = `${w.namespace}/${w.name}/${i}`;
    const healthy = i < w.replicas.ready;
    const name =
      w.kind === "StatefulSet" || w.kind === "DaemonSet"
        ? `${w.name}-${i}`
        : `${w.name}-${suffix(seed + "rs", 10)}-${suffix(seed + "p", 5)}`;
    const phase = healthy ? "Running" : w.issues.includes("Pending") ? "Pending" : "Running";
    pods.push({
      ref: { clusterId: "prod-seoul", namespace: w.namespace, podName: name, podUid: uid(seed) },
      name,
      uid: uid(seed),
      namespace: w.namespace,
      phase,
      severity: healthy ? "healthy" : w.severity,
      ready: healthy ? "1/1" : "0/1",
      restarts: healthy ? 0 : w.restarts,
      node: `ip-10-0-${int(seed + "n", 1, 60)}-${int(seed + "n2", 1, 250)}`,
      owner: owner,
      issues: podIssues(w, healthy),
      usage: usage(seed),
      startedAt: new Date(NOW_MS - int(seed + "s", 600, 400000) * 1000).toISOString(),
    });
  }

  /* 재생성 인스턴스 — 이름은 같고 UID가 다릅니다. 화면에서 반드시 구분되어야 합니다. */
  if (w.issues.includes("CrashLoopBackOff") && pods[0]) {
    const dead = pods[0]!;
    pods.push({
      ...dead,
      uid: uid(`${w.namespace}/${w.name}/prev`),
      ref: { ...dead.ref, podUid: uid(`${w.namespace}/${w.name}/prev`) },
      phase: "Failed",
      severity: "critical",
      ready: "0/1",
      startedAt: new Date(NOW_MS - 5400 * 1000).toISOString(),
      finishedAt: new Date(NOW_MS - 2500 * 1000).toISOString(),
    });
  }
  return pods;
}

export function containersOf(pod: PodSummary, w?: WorkloadSummary): ContainerStatus[] {
  const seed = pod.uid;
  const image = w?.images[0] ?? `registry.internal/${pod.name.split("-")[0]}:1.0.0`;
  const bad = pod.issues.length > 0 || pod.phase === "Failed";
  const app: ContainerStatus = {
    name: "app",
    image,
    imageId: `sha256:${suffix(seed + "img", 12)}${suffix(seed + "img2", 12)}`,
    ready: !bad,
    started: true,
    restarts: pod.restarts,
    state: bad ? "Waiting" : "Running",
    reason: bad ? (pod.issues[0] ?? "CrashLoopBackOff") : undefined,
    message: bad
      ? pod.issues.includes("ImagePullBackOff")
        ? `Failed to pull image "${image}": not found`
        : "Back-off restarting failed container"
      : undefined,
    lastTerminated:
      pod.restarts > 0
        ? {
            reason: pod.issues.includes("OOMKilled") ? "OOMKilled" : "Error",
            exitCode: pod.issues.includes("OOMKilled") ? 137 : 2,
            finishedAt: new Date(NOW_MS - int(seed + "t", 60, 3000) * 1000).toISOString(),
          }
        : undefined,
    usage: { cpuMilli: pod.usage.cpuMilli, memoryMib: pod.usage.memoryMib },
    probes: { liveness: bad ? "failing" : "passing", readiness: bad ? "failing" : "passing" },
  };
  /* 사이드카는 앱이 죽어도 대개 살아 있습니다. 원인 판단에 중요한 신호입니다. */
  const sidecar: ContainerStatus = {
    name: "otel-agent",
    image: "registry.internal/otel-agent:0.104.0",
    imageId: `sha256:${suffix(seed + "sc", 24)}`,
    ready: true,
    started: true,
    restarts: 0,
    state: "Running",
    usage: { cpuMilli: 24, memoryMib: 96 },
    probes: { liveness: "passing", readiness: "passing" },
  };
  return [app, sidecar];
}

/* ── Namespace 요약 ──────────────────────────────────────────────────────── */

export function namespaceSummary(ns: string): NamespaceSummary {
  const ws = workloadsOf(ns);
  const pods = ws.reduce((s, w) => s + Math.max(w.replicas.desired, w.replicas.ready), 0);
  const ready = ws.reduce((s, w) => s + w.replicas.ready, 0);
  const failed = ws.filter((w) => w.severity === "critical").length;
  const unhealthy = ws.filter((w) => w.severity !== "healthy").length;
  const issues = [...new Set(ws.flatMap((w) => w.issues))];
  const agg = ws.reduce(
    (acc, w) => {
      acc.cpuMilli += w.usage.cpuMilli;
      acc.cpuRequestMilli += w.usage.cpuRequestMilli;
      acc.memoryMib += w.usage.memoryMib;
      acc.memoryRequestMib += w.usage.memoryRequestMib;
      return acc;
    },
    { cpuMilli: 0, cpuRequestMilli: 0, memoryMib: 0, memoryRequestMib: 0 },
  );
  return {
    name: ns,
    severity: ws.some((w) => w.severity === "critical")
      ? "critical"
      : ws.some((w) => w.severity === "degraded")
        ? "degraded"
        : unhealthy
          ? "warning"
          : "healthy",
    workloads: { total: ws.length, unhealthy },
    pods: { total: pods, running: ready, pending: Math.max(0, pods - ready - failed), failed, restarts: ws.reduce((s, w) => s + w.restarts, 0) },
    usage: {
      ...agg,
      cpuLimitMilli: null,
      memoryLimitMib: null,
      cpuVsRequest: agg.cpuMilli / Math.max(1, agg.cpuRequestMilli),
      cpuVsLimit: null,
      memoryVsRequest: agg.memoryMib / Math.max(1, agg.memoryRequestMib),
      memoryVsLimit: null,
    },
    issues,
  };
}

/* ── 추세 ────────────────────────────────────────────────────────────────── */

export function scopedTrends(key: RangeKey, seedPrefix: string): TrendPanel[] {
  const { buckets, stepSeconds } = rangeWindow(key);
  const mk = (seed: string, base: number, amp: number) => {
    const noiseAmp = amp / Math.sqrt(stepSeconds / 60);
    return Array.from({ length: buckets }, (_, i) => ({
      t: NOW_MS - (buckets - 1 - i) * stepSeconds * 1000,
      v: Math.max(
        0,
        base + Math.sin((i / buckets) * Math.PI * 2.2) * amp * 0.6 + (hash(`${seed}|${i}`) - 0.5) * 2 * noiseAmp,
      ),
    }));
  };
  return [
    {
      id: "cpu",
      title: "CPU 사용률",
      stepSeconds,
      series: [
        { key: "req", label: "Request 대비", unit: "percent", points: mk(seedPrefix + "cpu-req", 78, 14) },
        { key: "lim", label: "Limit 대비", unit: "percent", points: mk(seedPrefix + "cpu-lim", 44, 10) },
      ],
    },
    {
      id: "memory",
      title: "Memory 사용률",
      stepSeconds,
      series: [
        { key: "req", label: "Request 대비", unit: "percent", points: mk(seedPrefix + "mem-req", 86, 9) },
        { key: "lim", label: "Limit 대비", unit: "percent", points: mk(seedPrefix + "mem-lim", 52, 7) },
      ],
    },
    {
      id: "restarts",
      title: "재시작",
      stepSeconds,
      series: [{ key: "restarts", label: "재시작", unit: "count", points: mk(seedPrefix + "rst", 0.8, 0.9) }],
    },
  ];
}
