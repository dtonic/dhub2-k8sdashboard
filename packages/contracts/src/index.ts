import type { DashboardDefinition } from "@k8s-dashboard/dashboard-schema";

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
  containerName?: string;
  /** OpenTelemetry service.* 대응. namespace/version은 serviceName이 있어야 의미가 있습니다. */
  serviceName?: string;
  serviceNamespace?: string;
  serviceVersion?: string;
}

/* ── Unified Telemetry Model (이슈 #4) ──────────────────────────────────── */
export * from "./telemetry";

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
  /** Query Catalog의 unit 값과 1:1입니다. 값은 원시 단위이며 표시는 UI가 자동 환산합니다. (#31) */
  unit: "percent" | "bytes" | "bytes_per_sec" | "count" | "cores" | "millicores" | "mebibytes";
  points: TrendPoint[];
}

export interface TrendPanel {
  id: "cpu" | "memory" | "network" | "io" | "restarts";
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
/* ── Workload/Secret 관리 (ADR 0014, #32) ─────────────────────────────── */

export interface ManagedWorkload {
  namespace: string;
  name: string;
  kind: "Deployment" | "Secret";
  ready: number;
  desired: number;
  secretType?: string;
  updatedAt: string;
}

export interface ManagedWorkloadListResponse {
  clusterId: string;
  generatedAt: string;
  items: ManagedWorkload[];
}

export interface ManagedPod {
  name: string;
  uid: string;
  namespace: string;
  phase: string;
  ready: boolean;
  restarts: number;
  severity: Severity;
}

export interface ManagedDeploymentDetail {
  clusterId: string;
  namespace: string;
  name: string;
  generatedAt: string;
  ready: number;
  desired: number;
  /** managedFields·status를 제거한 관리자용 JSON 매니페스트 문자열. */
  manifest: string;
  pods: ManagedPod[];
}

export interface ManagedSecretDetail {
  clusterId: string;
  namespace: string;
  name: string;
  generatedAt: string;
  secretType: string;
  /** key → 평문 값(서버가 base64 디코딩). ADR 0014 — admin 전용 노출. */
  data: Record<string, string>;
  pods: ManagedPod[];
}

export interface ManagedActionResult {
  ok: boolean;
  message: string;
  affected?: string[];
}

export interface ScopeResponse {
  clusters: Array<{
    id: string;
    name: string;
    /** 이 클러스터에서 사용자가 접근 가능한 namespace. "all"이면 전체 */
    namespaces: string[] | "all";
    /** 접근 가능한 namespace가 하나도 없으면 false. 목록에는 보이되 선택 불가 */
    accessible: boolean;
    /**
     * namespaces가 "all"일 때 셀렉터 옵션으로 쓸 실제 namespace 이름 목록(서버
     * informer 캐시 열거). 표시 힌트일 뿐 권한 판정은 서버가 요청마다 강제합니다. (#1)
     */
    availableNamespaces?: string[];
  }>;
  /** Deployment/Secret 관리 탭·버튼 노출 여부(platform.admin + 관리 기능 활성). (ADR 0014) */
  canManageWorkloads?: boolean;
  /**
   * Resource Explorer 진입점 노출 여부. platform.admin에서 파생한 capability이면서
   * 이 배포에 direct 모드 resource 서비스가 있을 때만 true입니다. (ADR 0018)
   */
  canExploreResources?: boolean;
}

/* ── Resource Explorer (ADR 0018) ───────────────────────────────────────────
   조회 전용입니다. 목록은 서버의 metadata informer 인덱스에서만 나오고,
   상세만 사용자가 항목을 연 순간의 격리된 live GET입니다. */

/** allowlist 한 항목의 상태. "0건"과 "권한 없음"과 "미지원"을 구분합니다. */
export type ResourceState = "ready" | "syncing" | "unsupported" | "forbidden" | "missing";

export interface ResourceDescriptor {
  /** core group은 "core"로 표기합니다. 경로 세그먼트가 비어 있을 수 없기 때문입니다. */
  group: string;
  version: string;
  resource: string;
  kind: string;
  namespaced: boolean;
  verbs: string[];
  /** 서버가 선호하는 group 버전(진단용). */
  preferredVersion?: string;
  state: ResourceState;
  /** ready가 아닐 때의 짧은 사유. 내부 주소·질의는 담기지 않습니다. */
  reason?: string;
  /** 로컬 인덱스에 담긴 객체 수(필터 전). */
  count: number;
  /**
   * 이 GVR에 변경 검토(dry-run)를 요청할 수 있는지. **권한이 아니라 배포 설정**이며,
   * 기능 스위치·opt-in 목록·hard-deny를 모두 통과할 때만 true입니다. (ADR 0019)
   * false면 UI는 검토 진입점을 만들지 않습니다.
   */
  dryRun?: boolean;
}

export interface ResourceCatalogResponse {
  clusterId: string;
  generatedAt: string;
  /** discovery snapshot을 마지막으로 만든 시각. */
  refreshedAt?: string;
  /** discovery 자체가 부분/전체 실패했는지. */
  degraded: boolean;
  reason?: string;
  items: ResourceDescriptor[];
}

/** 목록 한 줄. 신원은 이름이 아니라 UID입니다. */
export interface ResourceListItem {
  namespace?: string;
  name: string;
  uid: string;
  createdAt?: string;
}

export interface ResourceListResponse {
  clusterId: string;
  group: string;
  version: string;
  resource: string;
  kind: string;
  namespaced: boolean;
  generatedAt: string;
  /** 인덱스를 마지막으로 만든 시각. */
  observedAt?: string;
  appliedScope: { clusterId: string; namespaces: string[] | "all" };
  items: ResourceListItem[];
  /** opaque keyset cursor. offset 페이징은 어떤 요청에도 없습니다. (ADR 0003) */
  nextCursor?: string;
  /** 페이지·byte·scan 예산으로 조기 종료했는지. */
  truncated: boolean;
  /** 인덱스 전체 객체 수(필터 전). */
  total: number;
}

export interface ResourceDetailResponse {
  clusterId: string;
  group: string;
  version: string;
  resource: string;
  apiVersion: string;
  kind: string;
  namespace?: string;
  name: string;
  uid: string;
  resourceVersion: string;
  generatedAt: string;
  /** 서버가 정제한 읽기 전용 YAML. Secret의 data/stringData는 들어 있지 않습니다. */
  yaml: string;
  /** 제거한 필드 경로. 가려졌다는 사실은 보이게 합니다. */
  redacted?: string[];
}

/* ── 전역 리소스 검색 (ADR 0023) ─────────────────────────────────────────────
   ADR 0018 metadata 인덱스 위의 조회 전용 접두사 검색입니다.
   Scope는 후보 순회 전에 적용되므로 권한 밖 객체는 truncated·cursor에도
   영향을 주지 않습니다. namespace 파라미터가 없는 것이 계약입니다. */

/** 결과가 걸린 필드. 색이 아니라 텍스트로 함께 표시합니다. */
export type ResourceMatchField = "name" | "namespace" | "kind" | "label";

/**
 * 검색 결과 한 줄.
 *
 * **status 필드는 없습니다** — 검색은 PartialObjectMetadata 인덱스에서만 읽고
 * 그 안에 status가 없으므로, 있는 척하는 필드를 계약에 만들지 않습니다.
 */
export interface ResourceSearchItem {
  group: string;
  version: string;
  resource: string;
  kind: string;
  namespaced: boolean;
  namespace?: string;
  name: string;
  uid: string;
  matchedField: ResourceMatchField;
}

export interface ResourceSearchResponse {
  clusterId: string;
  query: string;
  generatedAt: string;
  /** 인덱스를 마지막으로 만든 시각. */
  observedAt?: string;
  appliedScope: { clusterId: string; namespaces: string[] | "all" };
  items: ResourceSearchItem[];
  /** 질의어와 Scope에 묶인 opaque keyset cursor. */
  nextCursor?: string;
  /** 페이지·byte·scan 예산으로 조기 종료했는지. Scope 밖 객체는 영향을 주지 않습니다. */
  truncated: boolean;
  /** 색인 예산으로 일부 리소스나 label이 빠졌는지. 잘린 검색을 완전한 검색처럼 보여주지 않습니다. */
  degraded: boolean;
  reason?: string;
}

/** 서버가 다시 확인해 준 최근 항목. 제목의 근거는 전부 서버 값입니다. */
export interface ResourceRecentItem {
  group: string;
  version: string;
  resource: string;
  kind: string;
  namespaced: boolean;
  namespace?: string;
  name: string;
  uid: string;
}

/**
 * 재해석된 최근 항목. 요청 순서를 그대로 지킵니다.
 *
 * 참조는 compact base64url이며 요청당 최대 20개, 참조 하나는 1024자, query string
 * 전체는 8KiB가 상한입니다. 웹은 6KiB에서 요청을 나누고 하나의 취소 신호를
 * 공유하며 입력 순서대로 합칩니다. 해석되지 않은 참조는 오류가 아니라 목록에서 빠집니다.
 */
export interface ResourceRecentResponse {
  clusterId: string;
  generatedAt: string;
  appliedScope: { clusterId: string; namespaces: string[] | "all" };
  items: ResourceRecentItem[];
}

/* ── 변경 검토 dry-run (ADR 0019 Phase 1) ────────────────────────────────────
   ADR 0018 Explorer 위에 얹는 **검토 전용** 계약입니다.

   적용·삭제·생성·change token·force·범용 write proxy는 **Phase 1 범위에
   없습니다(deferred)** — 기존 Deployment/Secret write 경로는 그대로 보존되고,
   뒤 단계에서 무엇을 되살릴지는 별도 ADR의 결정입니다. 응답에 매니페스트 원문·
   dry-run 객체·Secret 값·Kubernetes Status 원문이 없는 것은 이 계약 자체의
   고정 사항입니다 — raw 매니페스트는 브라우저→BFF 요청 본문에만 존재하고
   응답·오류·로그로 되돌아오지 않습니다.

   정본 경로: POST /api/v1/clusters/{clusterId}/resources/{group}/{version}/{resource}/object/dry-run

   서버가 강제하는 것: 본문 apiVersion·kind·namespace·name·uid·resourceVersion을
   선택 GVR·매니페스트·서버 Scope와 전부 대조, Secret·serviceaccounts는 설정과
   무관하게 거부, 나머지 hard-deny·미등록 GVR·기능 비활성·central 모드는 fail-closed,
   매니페스트는 단일 문서·중복 키 거부, 현재본과 결과 양쪽을 정제한 뒤 유계·결정적
   diff, live GET과 patch 본문 양쪽에서 UID/resourceVersion 재검증. */

/** 검토 대상 하나와 그 대상의 apply configuration. 동사·force·token은 없습니다. */
export interface ResourceDryRunRequest {
  /** 경로 GVR·매니페스트와 정확히 일치해야 합니다. */
  apiVersion: string;
  kind: string;
  /** namespaced면 필수, cluster 범위면 비어야 합니다. 힌트가 아니라 대조 대상입니다. */
  namespace?: string;
  name: string;
  /** 목록에서 본 UID. 다르면 Kubernetes로 나가지 않고 409입니다. */
  uid: string;
  /** CAS 기준. live GET 결과와 대조하고 patch 본문에도 실립니다. */
  resourceVersion: string;
  /** YAML/JSON 단일 문서. 기본 상한 256KiB, 절대 상한 1MiB. 초과 시 413. */
  manifest: string;
}

/** 변경 종류. 색이 아니라 텍스트로 함께 표시합니다. */
export type ResourceDryRunChangeOp = "added" | "removed" | "changed";

/** 정제된 현재본과 정제된 dry-run 결과의 leaf 차이 하나. */
export interface ResourceDryRunChange {
  /** canonical 경로. 배열은 name 키 또는 인덱스입니다. */
  path: string;
  op: ResourceDryRunChangeOp;
  before?: string;
  after?: string;
  /** 정제가 값 비교에서 뺀 경로. 바뀌었다는 사실만 알리고 값은 없습니다. */
  valueRedacted?: boolean;
  /** 값이 512바이트에서 잘렸는지. */
  valueTruncated?: boolean;
}

/**
 * 거절 사유 하나. Kubernetes Status 원문(message·reason·details·causes[].field·
 * causes[].message·code)은 **어느 필드로도 나가지 않습니다.** 서버는 Causes에서
 * 개수와 타입만 읽고 자신이 쓴 고정 문장만 보여 줍니다.
 */
export interface ResourceDryRunViolation {
  /**
   * 거절된 필드 경로. **Phase 1에서는 언제나 비어 있습니다** — causes[].field는
   * 구조적으로 보이지만 서버(특히 admission webhook)가 자유롭게 채우는 문자열이라
   * 객체 값·내부 경로가 실려 올 수 있습니다.
   */
  field?: string;
  /** 서버가 쓴 고정 문장. upstream 문자열이 아닙니다. */
  message: string;
  /**
   * 그 필드를 소유한 fieldManager. **Phase 1에서는 언제나 비어 있습니다** —
   * 소유자 이름이 전용 필드로 오지 않고 사람이 읽는 메시지 안에 섞여 오기
   * 때문입니다. field·manager 모두 신뢰할 수 있는 타입 필드가 생기면 채웁니다.
   */
  manager?: string;
}

/**
 * 검토 결과. `rejected`는 "검토가 성공적으로 끝났고 그 답이 거절"이며,
 * 요청·신원·정책 실패(4xx)와 다릅니다.
 */
export type ResourceDryRunOutcome = "unchanged" | "changed" | "rejected";

/** 거절 주체. force는 어떤 경우에도 지원하지 않습니다. */
export type ResourceDryRunRejectedBy = "validation" | "admission" | "conflict";

export interface ResourceDryRunResponse {
  clusterId: string;
  group: string;
  version: string;
  resource: string;
  apiVersion: string;
  kind: string;
  namespace?: string;
  name: string;
  uid: string;
  resourceVersion: string;
  generatedAt: string;
  /** 항상 "k8s-dashboard-dryrun". 계약이 상수로 못박습니다. */
  fieldManager: string;
  outcome: ResourceDryRunOutcome;
  rejectedBy?: ResourceDryRunRejectedBy;
  /**
   * canonical 경로 오름차순 정렬 후 최대 200개. 같은 정제 diff 집합은 항상 같은
   * 순서의 changes를 냅니다 — 응답 전체는 generatedAt 때문에 바이트가 달라집니다.
   */
  changes: ResourceDryRunChange[];
  /** 절단 이전 전체 변경 수. changes.length보다 클 수 있습니다. */
  changeCount: number;
  truncated: boolean;
  /**
   * **서버가 직접 쓴 고정 문장.** API 서버 Warning 헤더의 원문은 어떤 경우에도
   * 들어오지 않습니다 — admission webhook이 쓰는 그 문자열에는 대상 객체의 필드
   * 값이나 정책 세부가 실려 올 수 있습니다. 경고가 있었다는 사실만 전달하며,
   * 개수도 헤더에서 오지 않습니다.
   */
  warnings: string[];
  violations: ResourceDryRunViolation[];
  /** 정제로 비교에서 제외한 경로. 가려졌다는 사실은 보이게 합니다. */
  redacted: string[];
}

export type AuthSessionResponse =
  | { authenticated: false }
  | { authenticated: false; refreshable: true; csrfToken: string }
  | {
      authenticated: true;
      principal: { displayName: string };
      capabilities: { canEditDashboard: boolean; canPublishDashboard: boolean };
      expiresAt: string;
      refreshAt: string;
      csrfToken: string;
    };

export interface DashboardCapabilities { enabled:boolean; canEdit:boolean; canPublish:boolean; maxDrafts:number; maxWidgets:number; }
export type DashboardDraftState = "draft" | "submitted" | "approved";
export interface DashboardDraft { id:string; revision:number; state:DashboardDraftState; owned:boolean; schemaVersion:1; definition:DashboardDefinition; createdAt:string; updatedAt:string; }
export interface DashboardDraftPage { items:DashboardDraft[]; nextCursor?:string; }
export interface DashboardDefinitionRequest { definition:DashboardDefinition; }

/** 화면 전체가 실패했을 때만 쓰는 최상위 에러. 섹션 단위 실패는 Section으로 표현합니다. */
export interface ApiError {
  code:
    | "unauthorized"
    | "forbidden"
    | "not_found"
    | "method_not_allowed"
    | "upstream_unavailable"
    | "invalid_range"
    | "invalid_dashboard"
    | "invalid_revision"
    | "invalid_page"
    | "invalid_cursor"
    | "precondition_required"
    | "revision_conflict"
    | "draft_limit"
    | "approved_immutable"
    | "invalid_state"
    | "unsupported_media_type"
    | "body_too_large"
    | "dashboard_store_unavailable"
	| "bad_request"
	| "auth_unavailable"
	| "refresh_conflict"
	| "auth_rate_limited"
	| "session_unavailable"
	| "cluster_access_denied"
	| "namespace_access_denied"
	| "cluster_scope_required"
	| "resources_unavailable"
	| "resource_not_allowlisted"
	| "resource_not_served"
	| "resource_unsupported"
	| "resource_forbidden"
	| "resource_syncing"
	| "invalid_filter"
	| "detail_rate_limited"
	| "uid_mismatch"
	| "object_too_large"
	| "upstream_timeout"
	/* 변경 검토 dry-run (ADR 0019 Phase 1). 서버가 이미 내보내는 코드입니다 —
	   전부 "검토 요청이 성립하지 않았다"는 사유이고 쓰기 결과가 아닙니다. */
	| "dryrun_unavailable"
	| "dryrun_resource_denied"
	| "dryrun_rate_limited"
	| "dryrun_forbidden"
	| "invalid_manifest"
	| "manifest_mismatch"
	| "manifest_too_large"
	| "resource_version_mismatch"
    | "internal";
  message: string;
  /** 응답 헤더 X-Request-ID와 항상 같은 값. 문의·로그 대조는 이 값 하나로 합니다. */
  requestId: string;
}

/** GET /version 응답. 값은 빌드 시 주입되며 로컬 기본값은 dev/unknown입니다. */
export interface VersionInfo {
  version: string;
  commit: string;
  buildDate: string;
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

/** Nodes 화면의 노드별 Pod 행. 신원은 UID입니다. */
export interface NodePodSummary {
  uid: string;
  name: string;
  namespace: string;
  phase: string;
  severity: Severity;
  restarts: number;
  cpuRequestMilli: number;
  memoryRequestMib: number;
}

export interface NodeCapacity {
  cpuMilli: number;
  memoryMib: number;
  pods: number;
}

export interface NodeRequested {
  cpuMilli: number;
  memoryMib: number;
}

/**
 * Nodes 화면의 노드 하나 요약. requested/limits와 pods 목록은 스케줄러 관점
 * (종료되지 않은 Pod 전체)입니다. 실측 사용량은 메트릭 데이터소스 몫입니다.
 */
export interface NodeSummary {
  name: string;
  roles: string[];
  ready: boolean;
  unschedulable: boolean;
  pressure: boolean;
  severity: Severity;
  kubeletVersion: string;
  osImage: string;
  internalIP: string;
  ageSeconds: number;
  capacity: NodeCapacity;
  allocatable: NodeCapacity;
  requested: NodeRequested;
  limits: NodeRequested;
  podsTotal: number;
  pods: NodePodSummary[];
}

/** Nodes 화면 응답. 화면 하나 = 요청 하나. (ADR 0002) */
export interface NodeListResponse {
  clusterId: string;
  generatedAt: string;
  nodes: Section<NodeSummary[]>;
}

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
  /** 이 Pod가 참조하는 Secret 이름(값 아님). Secret 조회 카드에 씁니다. (#33) */
  secretRefs: string[];
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

/* ══════════════════════════════════════════════════════════════════════════
   Logs Explorer & 상관분석 — 이슈 #16
   ══════════════════════════════════════════════════════════════════════════ */

export type LogLevel = "ERROR" | "WARN" | "INFO" | "DEBUG";

/** 마스킹된 구간. UI가 원문을 복원할 수 없도록 위치와 종류만 내려옵니다. */
export interface MaskedSpan {
  /** message 내 시작 offset */
  start: number;
  length: number;
  kind: "token" | "password" | "secret" | "email" | "ip" | "card";
}

export interface LogLine {
  /** 정렬·커서에 쓰는 안정적 식별자 */
  id: string;
  /** epoch milliseconds */
  t: number;
  level: LogLevel;
  /** 이미 마스킹된 본문. 원문은 서버 밖으로 나가지 않습니다. (README §10) */
  message: string;
  /** 어디가 가려졌는지. UI가 표시만 하고 복원하지는 않습니다. */
  masked: MaskedSpan[];
  namespace: string;
  podName: string;
  podUid: string;
  containerName: string;
  workloadKind?: WorkloadKind;
  workloadName?: string;
  nodeName?: string;
  traceId?: string;
  spanId?: string;
  /** 구조화 로그의 부가 필드 */
  attributes?: Record<string, string>;
}

/**
 * Cursor 기반 페이징.
 * Quickwit의 search-after와 같은 방식입니다. offset을 쓰지 않는 이유는
 * 로그가 계속 들어오는 동안 offset이 밀려 **중복·누락**이 생기기 때문입니다.
 * 커서는 (timestamp, id) 복합키를 불투명 문자열로 인코딩한 값입니다.
 */
export interface LogCursor {
  /** 다음 페이지 요청에 그대로 넣습니다. null이면 더 없음 */
  next: string | null;
  /** 서버가 적용한 페이지 크기 */
  pageSize: number;
}

export interface LogSearchRequest {
  clusterId: string;
  namespace?: string;
  workloadName?: string;
  podUid?: string;
  containerName?: string;
  levels?: LogLevel[];
  /** 전문 검색어. Raw Query가 아니라 서버가 이스케이프해 처리합니다. */
  q?: string;
  from: string;
  to: string;
  cursor?: string;
}

export interface LogSearchResponse {
  lines: Section<LogLine[]>;
  cursor: LogCursor;
  /** 시간 범위 전체의 레벨별 분포. 히스토그램과 필터 배지에 씁니다. */
  histogram: Section<Array<{ t: number; counts: Record<LogLevel, number> }>>;
  /** 같은 Scope·시간 범위의 Kubernetes Event. 로그와 같은 타임라인에 겹칩니다. */
  events: Section<ClusterEvent[]>;
  /** 필터 드롭다운을 채우는 값들. 현재 Scope에서 실제로 관측된 것만 내려옵니다. */
  facets: Section<{
    workloads: Array<{ name: string; kind: WorkloadKind; count: number }>;
    pods: Array<{ name: string; uid: string; count: number }>;
    containers: Array<{ name: string; count: number }>;
  }>;
  /** 서버가 적용한 Scope와 범위. UI가 보낸 값과 다를 수 있습니다. */
  applied: {
    clusterId: string;
    namespace: string | null;
    from: string;
    to: string;
    /** 결과 상한에 걸렸는지. 걸렸다면 화면에 명시합니다. */
    truncated: boolean;
    maxLines: number;
  };
  generatedAt: string;
}

export const LEVEL_ORDER: LogLevel[] = ["ERROR", "WARN", "INFO", "DEBUG"];

export const MASK_LABEL: Record<MaskedSpan["kind"], string> = {
  token: "토큰",
  password: "비밀번호",
  secret: "시크릿",
  email: "이메일",
  ip: "IP",
  card: "카드번호",
};

/* ══════════════════════════════════════════════════════════════════════════
   Pod Topology — 이슈 #16 후속 (design-system의 topology 컴포넌트를 앱에 연결)
   ══════════════════════════════════════════════════════════════════════════ */

export type Protocol = "HTTP" | "gRPC" | "TCP" | "UDP";

export interface TopologyNode {
  id: string;
  ref: EntityRef;
  name: string;
  namespace: string;
  severity: Severity;
  /** 레이아웃 열(0부터). 서버가 의존 방향으로 위상 정렬해 내려줍니다. */
  column: number;
  row: number;
  /** 클러스터 밖 엔티티(Ingress Gateway·External Client 등). Pod 상세로 연결하지 않습니다. (#29) */
  external?: boolean;
  /** 이 노드로 접힌 Pod 수. 노드는 워크로드 단위이므로 접힘 규모를 표시합니다. (#3) */
  podCount?: number;
}

export interface TopologyRoute {
  protocol: Protocol;
  /** HTTP/gRPC는 API 경로, TCP/UDP는 라우트 식별자 */
  route: string;
  /** 선택된 시간 범위 누적 요청 수 */
  count: number;
  errorCount: number;
}

export interface TopologyEdge {
  id: string;
  from: string;
  to: string;
  severity: Severity;
  /** 시간 범위 전체 누적. 초당 환산은 UI가 계산합니다. */
  totalCount: number;
  errorRate: number;
  protocols: Protocol[];
  routes: TopologyRoute[];
}

/** 관리자가 저장한 노드 좌표. (#28) */
export interface TopologyNodePosition {
  id: string;
  x: number;
  y: number;
}

/** 모든 사용자가 공유하는 저장 배치. null이면 기본 배치를 씁니다. */
export interface TopologyLayout {
  positions: TopologyNodePosition[];
  updatedAt: string;
}

export interface TopologyResponse {
  clusterId: string;
  namespace: string | null;
  range: { key: RangeKey; from: string; to: string; stepSeconds: number };
  generatedAt: string;
  pods: Section<{ total: number; healthy: number; unhealthy: number; unhealthyList: UnhealthyEntity[] }>;
  graph: Section<{ nodes: TopologyNode[]; edges: TopologyEdge[] }>;
  layout: TopologyLayout | null;
  canEditLayout: boolean;
}

/** 엣지 하나의 시계열. 라인 클릭 후 차트로 전환할 때만 별도 조회합니다. */
export interface TopologyEdgeSeriesResponse {
  edgeId: string;
  range: { key: RangeKey; from: string; to: string; stepSeconds: number };
  generatedAt: string;
  series: Section<TrendSeries[]>;
}

/* ══════════════════════════════════════════════════════════════════════════
   Alerts — 이슈 #17
   자체 평가 엔진을 만들지 않습니다. Grafana Alerting / Alertmanager의 상태를
   공통 모델로 정규화해 조회만 합니다.
   ══════════════════════════════════════════════════════════════════════════ */

export type AlertSeverity = "critical" | "warning" | "info";
export type AlertStatus = "firing" | "resolved" | "pending";

export interface AlertInstance {
  /** 정규화된 fingerprint. 백엔드가 달라도 같은 알림은 같은 id를 갖습니다. */
  id: string;
  name: string;
  severity: AlertSeverity;
  status: AlertStatus;
  startsAt: string;
  endsAt?: string;
  labels: Record<string, string>;
  annotations: Record<string, string>;
  /** label에서 유추한 Unified Entity Model 참조. 못 찾으면 undefined. */
  entity?: EntityRef;
  entityName?: string;
  /** 원본 시스템 상세 화면. 새 탭으로 엽니다. */
  sourceUrl?: string;
  source: "grafana" | "alertmanager";
  /** 같은 그룹으로 묶인 인스턴스 수. 1이면 단독. */
  groupSize: number;
  /** 그룹 키 — 무엇을 기준으로 묶었는지 화면에 노출합니다. */
  groupKey: string;
}

export interface AlertListResponse {
  clusterId: string;
  range: { key: RangeKey; from: string; to: string; stepSeconds: number };
  generatedAt: string;
  /** 진행 중 알림. Alert backend가 죽으면 degraded로 내려오고 화면은 계속 동작합니다. */
  firing: Section<AlertInstance[]>;
  /** 선택 범위 안에서 해소된 알림 */
  resolved: Section<AlertInstance[]>;
  counts: Section<Record<AlertSeverity, { firing: number; resolved: number }>>;
  /** 중복 grouping 기준. 화면에 그대로 노출해 "왜 묶였는지"를 설명합니다. */
  groupingRule: string;
}

export const ALERT_SEVERITY_LABEL: Record<AlertSeverity, string> = {
  critical: "Critical",
  warning: "Warning",
  info: "Info",
};

export const ALERT_STATUS_LABEL: Record<AlertStatus, string> = {
  firing: "진행 중",
  resolved: "해소됨",
  pending: "평가 중",
};

/* ── 상태 변경 SSE (이슈 #12) ───────────────────────────────────────────── */

/**
 * SSE 무효화 대상 종류. `reset`은 데이터가 아니라 제어 신호로,
 * 브라우저가 현재 상태를 HTTP로 다시 조회해야 함을 뜻합니다.
 * 정본 스키마: packages/contracts/schema/stream.schema.json
 */
export type StreamEventKind = "pod" | "workload" | "kubeevent" | "alert" | "reset";

/** SSE 변경 종류. */
export type StreamEventAction = "added" | "updated" | "deleted" | "reset";

/**
 * `GET /api/v1/clusters/{clusterId}/events/stream` 각 SSE 메시지의 data 본문입니다.
 * 무효화 신호이며 raw Kubernetes 객체·Secret·Alert annotation·시계열 샘플은 싣지 않습니다.
 * `id`는 불투명 값입니다 — 재연결 시 Last-Event-ID로 되돌려 보내기만 합니다.
 * 서버 인스턴스가 바뀌었거나 보존 창을 벗어난 ID면 `kind: "reset"`이 내려옵니다.
 */
export interface EventEnvelope {
  id: string;
  kind: StreamEventKind;
  action: StreamEventAction;
  clusterId: string;
  observedAt: string;
  /** 비어 있으면 클러스터 전역 변경 — Scope All 구독자에게만 전달됩니다. */
  namespace?: string;
  entity?: EntityRef;
  resourceVersion?: string;
}
