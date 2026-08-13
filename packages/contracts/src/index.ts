/**
 * @k8s-dashboard/contracts
 * --------------------------------------------------------------------------
 * Observability API/BFF와 Web UI가 공유하는 응답 계약입니다.
 * 이후 OpenAPI에서 생성하도록 바꿀 수 있으나, 지금은 손으로 유지합니다.
 *
 * 설계 규칙
 * - **화면 단위 집계 응답.** Cluster Overview는 위젯마다 호출하지 않고
 *   `GET /api/v1/clusters/{clusterId}/overview` 하나로 받습니다. (이슈 #14 완료 기준:
 *   초기 로딩에서 N+1 요청이 발생하지 않을 것)
 * - **부분 장애를 값으로 표현합니다.** 패널 단위로 `Section<T>`를 쓰고, 한 데이터소스가
 *   죽어도 나머지 섹션은 정상 값을 유지합니다. 화면은 섹션별로 Degraded를 표시합니다.
 * - **권한 없음은 에러가 아니라 상태입니다.** `status: "forbidden"`으로 내려오고,
 *   화면은 "데이터 없음"과 다르게 렌더링합니다.
 * - Scope(cluster/namespace)는 요청 파라미터를 신뢰하지 않고 서버가 토큰에서 강제 삽입합니다.
 *   UI가 보내는 값은 힌트일 뿐입니다. (README §10)
 */

/* ── 공통 ────────────────────────────────────────────────────────────────── */

/** 시간 범위 프리셋. Custom의 최대 폭은 30일이며 서버가 강제합니다. */
export type RangeKey = "1h" | "1d" | "7d" | "30d" | "custom";

/** 범위별 강제 Step(초). 사용자가 고르지 않습니다. (README §11) */
export const STEP_SECONDS: Record<Exclude<RangeKey, "custom">, number> = {
  "1h": 60,
  "1d": 300,
  "7d": 900,
  "30d": 3600,
};

export const RANGE_LABEL: Record<RangeKey, string> = {
  "1h": "최근 1시간",
  "1d": "최근 1일",
  "7d": "최근 7일",
  "30d": "최근 30일",
  custom: "Custom",
};

export const STEP_LABEL: Record<Exclude<RangeKey, "custom">, string> = {
  "1h": "1분",
  "1d": "5분",
  "7d": "15분",
  "30d": "1시간",
};

/** 정규화된 심각도. 색과 아이콘은 design-system의 status 토큰에 대응합니다. */
export type Severity = "healthy" | "progressing" | "warning" | "degraded" | "critical" | "unknown";

/**
 * 패널 단위 봉투(envelope).
 * - ok        : 값이 있음
 * - empty     : 권한도 있고 조회도 성공했지만 결과가 0건
 * - forbidden : Scope 밖이라 조회 자체가 거절됨
 * - degraded  : 데이터소스 일부 장애. `data`가 있으면 stale 값이라도 보여줍니다.
 */
export type SectionStatus = "ok" | "empty" | "forbidden" | "degraded";

export interface Section<T> {
  status: SectionStatus;
  /** degraded일 때도 마지막으로 성공한 값이 있으면 함께 내려옵니다. */
  data?: T;
  /** 어떤 데이터소스가 문제인지. 화면 문구에 그대로 노출합니다. */
  source?: "greptimedb" | "quickwit" | "kubernetes" | "alertmanager";
  /** 사람이 읽는 사유. 스택트레이스나 내부 식별자를 담지 않습니다. */
  reason?: string;
  /** 이 섹션 값이 만들어진 시각(RFC3339). stale 판정에 씁니다. */
  observedAt?: string;
}

/* ── 엔티티 ──────────────────────────────────────────────────────────────── */

/** 식별 우선순위: Pod UID → Workload UID → ns+kind+name → Pod Name (README §5) */
export interface EntityRef {
  clusterId: string;
  namespace?: string;
  workloadKind?: "Deployment" | "StatefulSet" | "DaemonSet" | "ReplicaSet" | "CronJob";
  workloadName?: string;
  workloadUid?: string;
  podName?: string;
  podUid?: string;
}

/* ── Overview 섹션별 페이로드 ───────────────────────────────────────────── */

export interface NodeHealth {
  total: number;
  ready: number;
  notReady: number;
  /** MemoryPressure/DiskPressure/PIDPressure 중 하나라도 걸린 노드 수 */
  pressure: number;
  unschedulable: number;
}

export interface PodHealth {
  total: number;
  running: number;
  pending: number;
  failed: number;
  crashLoopBackOff: number;
  imagePullBackOff: number;
  /** 최근 범위 내 재시작 합계 */
  restarts: number;
}

export interface WorkloadHealth {
  total: number;
  available: number;
  /** desired와 ready replica가 다른 워크로드 수 */
  replicaMismatch: number;
  /** progressDeadline을 넘긴 롤아웃 */
  rolloutStalled: number;
}

export interface TrendPoint {
  /** epoch milliseconds */
  t: number;
  v: number;
}

export interface TrendSeries {
  key: string;
  label: string;
  unit: "percent" | "bytes" | "bytes_per_sec" | "count";
  points: TrendPoint[];
}

export interface TrendPanel {
  id: "cpu" | "memory" | "network" | "restarts";
  title: string;
  stepSeconds: number;
  series: TrendSeries[];
}

export interface UnhealthyEntity {
  ref: EntityRef;
  /** 표시용 이름. Pod가 있으면 Pod 이름, 없으면 Workload 이름 */
  name: string;
  kind: "Pod" | "Workload" | "Node";
  namespace: string;
  severity: Severity;
  /** 정규화된 사유. CrashLoopBackOff, Replica 2/3 등 */
  reason: string;
  restarts: number;
  /** 이 상태가 유지된 시간(초) */
  forSeconds: number;
}

export interface ClusterEvent {
  id: string;
  type: "Normal" | "Warning";
  reason: string;
  message: string;
  involved: EntityRef;
  involvedName: string;
  namespace: string;
  count: number;
  lastSeen: string;
}

export interface AlertSummary {
  /** Grafana Alerting/Alertmanager의 severity를 그대로 씁니다. */
  bySeverity: { critical: number; warning: number; info: number };
  /** 대표 알림 몇 건. 전체 목록은 Alert 화면에서 봅니다. */
  top: Array<{
    id: string;
    name: string;
    severity: "critical" | "warning" | "info";
    namespace: string;
    activeSince: string;
  }>;
}

export interface TopologyEdgeSummary {
  from: string;
  to: string;
  protocol: "HTTP" | "gRPC" | "TCP" | "UDP";
  requestsPerSecond: number;
  errorRate: number;
  severity: Severity;
}

export interface TopologySummary {
  pods: number;
  edges: number;
  /** 문제 있는 경로만 축약해서 보여줍니다. 전체는 Topology 화면. */
  problemEdges: TopologyEdgeSummary[];
}

/* ── 화면 단위 응답 ─────────────────────────────────────────────────────── */

export interface ClusterOverviewResponse {
  clusterId: string;
  clusterName: string;
  /** 서버가 강제 적용한 Scope. UI가 보낸 값과 다를 수 있습니다. */
  appliedScope: { clusterId: string; namespaces: string[] | "all" };
  range: { key: RangeKey; from: string; to: string; stepSeconds: number };
  generatedAt: string;

  nodes: Section<NodeHealth>;
  pods: Section<PodHealth>;
  workloads: Section<WorkloadHealth>;
  trends: Section<TrendPanel[]>;
  unhealthy: Section<UnhealthyEntity[]>;
  events: Section<ClusterEvent[]>;
  alerts: Section<AlertSummary>;
  topology: Section<TopologySummary>;
}

/** Scope Selector가 쓰는 목록. 사용자가 볼 수 있는 범위만 내려옵니다. */
export interface ScopeResponse {
  clusters: Array<{
    id: string;
    name: string;
    /** 이 클러스터에서 사용자가 접근 가능한 namespace. "all"이면 전체 */
    namespaces: string[] | "all";
    /** 접근 가능한 namespace가 하나도 없으면 false. 목록에는 보이되 선택 불가 */
    accessible: boolean;
  }>;
}

/** 화면 전체가 실패했을 때만 쓰는 최상위 에러. 섹션 단위 실패는 Section으로 표현합니다. */
export interface ApiError {
  code: "unauthorized" | "forbidden" | "upstream_unavailable" | "invalid_range" | "internal";
  message: string;
}

/* ══════════════════════════════════════════════════════════════════════════
   Drill-down — 이슈 #15
   Cluster → Namespace → Workload → Pod → Container
   화면 하나당 요청 하나 원칙(ADR 0002)을 그대로 따릅니다.
   ══════════════════════════════════════════════════════════════════════════ */

/** 리소스 사용량과 Request/Limit 대비 비율. 비율은 서버가 계산해 내려줍니다. */
export interface ResourceUsage {
  /** CPU millicore */
  cpuMilli: number;
  cpuRequestMilli: number;
  cpuLimitMilli: number | null;
  /** Memory MiB */
  memoryMib: number;
  memoryRequestMib: number;
  memoryLimitMib: number | null;
  /** 0~1 이상이면 과사용. limit이 없으면 null */
  cpuVsRequest: number;
  cpuVsLimit: number | null;
  memoryVsRequest: number;
  memoryVsLimit: number | null;
}

export type WorkloadKind = "Deployment" | "StatefulSet" | "DaemonSet" | "ReplicaSet" | "CronJob";

/** Cluster Overview에서 정규화한 상태 어휘를 그대로 씁니다. */
export type PodPhase = "Running" | "Pending" | "Succeeded" | "Failed" | "Unknown";

/** 필터 칩에 쓰이는 정규화 사유. 서버가 정규화해 내려줍니다. */
export type IssueReason =
  | "CrashLoopBackOff"
  | "ImagePullBackOff"
  | "Pending"
  | "ReplicaMismatch"
  | "RolloutStalled"
  | "Restarting"
  | "OOMKilled"
  | "ProbeFailed";

export interface NamespaceSummary {
  name: string;
  severity: Severity;
  workloads: { total: number; unhealthy: number };
  pods: { total: number; running: number; pending: number; failed: number; restarts: number };
  usage: ResourceUsage;
  /** 이 Namespace에서 관측된 문제 사유. 목록 화면 필터에 씁니다. */
  issues: IssueReason[];
}

export interface NamespaceListResponse {
  clusterId: string;
  range: { key: RangeKey; from: string; to: string; stepSeconds: number };
  generatedAt: string;
  namespaces: Section<NamespaceSummary[]>;
}

export interface WorkloadSummary {
  ref: EntityRef;
  name: string;
  kind: WorkloadKind;
  namespace: string;
  severity: Severity;
  /** DaemonSet은 desired가 노드 수입니다. */
  replicas: { desired: number; ready: number; available: number; updated: number };
  rollout: { status: "Complete" | "Progressing" | "Stalled" | "Paused"; message?: string };
  restarts: number;
  usage: ResourceUsage;
  images: string[];
  issues: IssueReason[];
  ageSeconds: number;
}

export interface NamespaceDetailResponse {
  clusterId: string;
  namespace: string;
  range: { key: RangeKey; from: string; to: string; stepSeconds: number };
  generatedAt: string;
  summary: Section<NamespaceSummary>;
  workloads: Section<WorkloadSummary[]>;
  trends: Section<TrendPanel[]>;
  events: Section<ClusterEvent[]>;
}

/** OwnerReference 체인. Deployment → ReplicaSet → Pod 관계를 그대로 표현합니다. */
export interface OwnerRef {
  kind: WorkloadKind | "Node" | "Job";
  name: string;
  uid: string;
  /** 현재 활성 ReplicaSet인지. 롤아웃 중에는 여러 개가 공존합니다. */
  current?: boolean;
  /** 이 소유자에 속한 Pod 수 */
  pods?: number;
  revision?: string;
}

export interface ContainerStatus {
  name: string;
  image: string;
  /** 이미지 태그가 아니라 실제 배포된 digest. 롤백 판단에 필요합니다. */
  imageId?: string;
  ready: boolean;
  started: boolean;
  restarts: number;
  state: "Running" | "Waiting" | "Terminated";
  /** Waiting/Terminated일 때의 사유. CrashLoopBackOff 등 */
  reason?: string;
  message?: string;
  lastTerminated?: { reason: string; exitCode: number; finishedAt: string };
  usage?: { cpuMilli: number; memoryMib: number };
  probes: { liveness: "passing" | "failing" | "none"; readiness: "passing" | "failing" | "none" };
}

export interface PodSummary {
  ref: EntityRef;
  name: string;
  /** Pod UID. 이름이 같아도 재생성되면 다른 인스턴스입니다. (이슈 #15 완료 기준) */
  uid: string;
  namespace: string;
  phase: PodPhase;
  severity: Severity;
  ready: string;
  restarts: number;
  node: string;
  /** 소유 체인의 직접 부모(대개 ReplicaSet) */
  owner?: OwnerRef;
  issues: IssueReason[];
  usage: ResourceUsage;
  startedAt: string;
  /** 이미 삭제된 인스턴스면 종료 시각이 있습니다. */
  finishedAt?: string;
}

export interface WorkloadDetailResponse {
  clusterId: string;
  namespace: string;
  range: { key: RangeKey; from: string; to: string; stepSeconds: number };
  generatedAt: string;
  workload: Section<WorkloadSummary>;
  /** Deployment → ReplicaSet 체인. DaemonSet/StatefulSet은 비어 있습니다. */
  ownerChain: Section<OwnerRef[]>;
  pods: Section<PodSummary[]>;
  trends: Section<TrendPanel[]>;
  events: Section<ClusterEvent[]>;
}

export interface PodDetailResponse {
  clusterId: string;
  namespace: string;
  range: { key: RangeKey; from: string; to: string; stepSeconds: number };
  generatedAt: string;
  pod: Section<PodSummary>;
  /** Pod → ReplicaSet → Deployment 순서의 상위 체인 */
  ownerChain: Section<OwnerRef[]>;
  containers: Section<ContainerStatus[]>;
  trends: Section<TrendPanel[]>;
  events: Section<ClusterEvent[]>;
}

export const ISSUE_LABEL: Record<IssueReason, string> = {
  CrashLoopBackOff: "CrashLoopBackOff",
  ImagePullBackOff: "ImagePullBackOff",
  Pending: "Pending",
  ReplicaMismatch: "Replica 불일치",
  RolloutStalled: "Rollout 지연",
  Restarting: "재시작 발생",
  OOMKilled: "OOMKilled",
  ProbeFailed: "Probe 실패",
};
