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
