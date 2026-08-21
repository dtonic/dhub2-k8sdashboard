import { keepPreviousData, useInfiniteQuery, useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import type {
  AlertListResponse,
  ClusterOverviewResponse,
  NamespaceDetailResponse,
  NamespaceListResponse,
  NodeListResponse,
  LogSearchResponse,
  ManagedActionResult,
  ManagedDeploymentDetail,
  ManagedSecretDetail,
  ManagedWorkloadListResponse,
  PodDetailResponse,
  RangeKey,
  ResourceCatalogResponse,
  ResourceDetailResponse,
  ResourceListResponse,
  ScopeResponse,
  TopologyEdgeSeriesResponse,
  TopologyLayout,
  TopologyNodePosition,
  TopologyResponse,
  WorkloadDetailResponse,
} from "@k8s-dashboard/contracts";
import { apiGet, apiRequest, HttpError } from "./client";

export const queryKeys = {
  scope: ["scope"] as const,
  overview: (clusterId: string, namespace: string, range: RangeKey) =>
    ["cluster-overview", clusterId, namespace, range] as const,
};

export function useScope() {
  return useQuery({
    queryKey: queryKeys.scope,
    queryFn: ({ signal }) => apiGet<ScopeResponse>("/api/v1/scope", {}, signal),
    staleTime: 5 * 60 * 1000,
  });
}

/* ── Deployment/Secret 관리 (ADR 0014, #32) ───────────────────────────── */

export const manageKeys = {
  list: (clusterId: string, kind: "deployments" | "secrets", ns: string) =>
    ["manage-list", clusterId, kind, ns] as const,
  detail: (clusterId: string, kind: "deployments" | "secrets", ns: string, name: string) =>
    ["manage-detail", clusterId, kind, ns, name] as const,
};

export function useManagedList(clusterId: string, kind: "deployments" | "secrets", ns: string, enabled: boolean) {
  return useQuery({
    queryKey: manageKeys.list(clusterId, kind, ns),
    queryFn: ({ signal }) =>
      apiGet<ManagedWorkloadListResponse>(
        `/api/v1/clusters/${encodeURIComponent(clusterId)}/${kind}`,
        ns && ns !== "all" ? { ns } : {},
        signal,
      ),
    enabled: enabled && Boolean(clusterId),
    retry: retryPolicy,
  });
}

export function useManagedDeployment(clusterId: string, ns: string, name: string, enabled: boolean) {
  return useQuery({
    queryKey: manageKeys.detail(clusterId, "deployments", ns, name),
    queryFn: ({ signal }) =>
      apiGet<ManagedDeploymentDetail>(
        `/api/v1/clusters/${encodeURIComponent(clusterId)}/deployments/${encodeURIComponent(ns)}/${encodeURIComponent(name)}`,
        {},
        signal,
      ),
    enabled: enabled && Boolean(clusterId && ns && name),
  });
}

export function useManagedSecret(clusterId: string, ns: string, name: string, enabled: boolean) {
  return useQuery({
    queryKey: manageKeys.detail(clusterId, "secrets", ns, name),
    queryFn: ({ signal }) =>
      apiGet<ManagedSecretDetail>(
        `/api/v1/clusters/${encodeURIComponent(clusterId)}/secrets/${encodeURIComponent(ns)}/${encodeURIComponent(name)}`,
        {},
        signal,
      ),
    enabled: enabled && Boolean(clusterId && ns && name),
  });
}

/** 관리 write(수정·재배포) 공통 mutation. 성공 시 관련 쿼리를 무효화합니다. */
export function useManageAction(clusterId: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (a: { method: "PUT" | "POST"; path: string; body?: unknown }) =>
      apiRequest<ManagedActionResult>(`/api/v1/clusters/${encodeURIComponent(clusterId)}/${a.path}`, {
        method: a.method,
        ...(a.body !== undefined ? { headers: { "Content-Type": "application/json" }, body: JSON.stringify(a.body) } : {}),
      }),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["manage-detail", clusterId] }),
  });
}

/**
 * Cluster Overview는 **한 번의 요청**으로 화면 전체를 받습니다.
 * 위젯마다 훅을 만들면 초기 로딩에서 N+1이 발생합니다. (이슈 #14 완료 기준)
 *
 * `placeholderData: keepPreviousData` 덕분에 범위/스코프를 바꿔도 화면이 비워지지 않고
 * 값만 교체됩니다. 자동 갱신도 같은 경로를 씁니다.
 */
export function useClusterOverview(args: {
  clusterId: string;
  namespace: string;
  range: RangeKey;
  refreshMs: number;
}) {
  const { clusterId, namespace, range, refreshMs } = args;
  return useQuery({
    queryKey: queryKeys.overview(clusterId, namespace, range),
    queryFn: ({ signal }) =>
      apiGet<ClusterOverviewResponse>(
        `/api/v1/clusters/${encodeURIComponent(clusterId)}/overview`,
        { range, ...(namespace === "all" ? {} : { namespace }) },
        signal,
      ),
    placeholderData: keepPreviousData,
    refetchInterval: refreshMs > 0 ? refreshMs : false,
    refetchOnWindowFocus: false,
    /* 권한 문제는 재시도해도 달라지지 않습니다. */
    retry: (count, error) => !(error instanceof HttpError) && count < 2,
    staleTime: 10_000,
  });
}

/* ══════════════════════════════════════════════════════════════════════════
   Drill-down (이슈 #15) — 화면 하나당 요청 하나. ADR 0002를 그대로 따릅니다.
   ══════════════════════════════════════════════════════════════════════════ */

/** 권한 문제는 재시도해도 결과가 같습니다. 404도 마찬가지입니다. */
const retryPolicy = (count: number, error: unknown) =>
  !(error instanceof HttpError && (error.status === 403 || error.status === 404)) && count < 2;

const common = {
  placeholderData: keepPreviousData,
  refetchOnWindowFocus: false,
  staleTime: 10_000,
  retry: retryPolicy,
} as const;

export const drillKeys = {
  nodes: (clusterId: string) => ["nodes", clusterId] as const,
  namespaces: (clusterId: string, range: RangeKey) => ["namespaces", clusterId, range] as const,
  namespace: (clusterId: string, ns: string, range: RangeKey) => ["namespace", clusterId, ns, range] as const,
  workload: (clusterId: string, ns: string, kind: string, name: string, range: RangeKey) =>
    ["workload", clusterId, ns, kind, name, range] as const,
  /** Pod는 UID가 신원입니다. 이름이 같아도 재생성되면 다른 캐시 항목입니다. */
  pod: (clusterId: string, ns: string, name: string, uid: string, range: RangeKey) =>
    ["pod", clusterId, ns, name, uid, range] as const,
};

export function useNodeList(clusterId: string, refreshMs: number) {
  return useQuery({
    ...common,
    queryKey: drillKeys.nodes(clusterId),
    queryFn: ({ signal }) => apiGet<NodeListResponse>(`/api/v1/clusters/${encodeURIComponent(clusterId)}/nodes`, {}, signal),
    refetchInterval: refreshMs > 0 ? refreshMs : false,
  });
}

export function useNamespaceList(clusterId: string, range: RangeKey, refreshMs: number) {
  return useQuery({
    ...common,
    queryKey: drillKeys.namespaces(clusterId, range),
    queryFn: ({ signal }) =>
      apiGet<NamespaceListResponse>(`/api/v1/clusters/${encodeURIComponent(clusterId)}/namespaces`, { range }, signal),
    refetchInterval: refreshMs > 0 ? refreshMs : false,
  });
}

export function useNamespaceDetail(clusterId: string, ns: string, range: RangeKey, refreshMs: number) {
  return useQuery({
    ...common,
    queryKey: drillKeys.namespace(clusterId, ns, range),
    queryFn: ({ signal }) =>
      apiGet<NamespaceDetailResponse>(
        `/api/v1/clusters/${encodeURIComponent(clusterId)}/namespaces/${encodeURIComponent(ns)}`,
        { range },
        signal,
      ),
    refetchInterval: refreshMs > 0 ? refreshMs : false,
    enabled: Boolean(ns),
  });
}

export function useWorkloadDetail(
  clusterId: string,
  ns: string,
  kind: string,
  name: string,
  range: RangeKey,
  refreshMs: number,
) {
  return useQuery({
    ...common,
    queryKey: drillKeys.workload(clusterId, ns, kind, name, range),
    queryFn: ({ signal }) =>
      apiGet<WorkloadDetailResponse>(
        `/api/v1/clusters/${encodeURIComponent(clusterId)}/workloads/${encodeURIComponent(kind)}/${encodeURIComponent(name)}`,
        { ns, range },
        signal,
      ),
    refetchInterval: refreshMs > 0 ? refreshMs : false,
    enabled: Boolean(ns && name),
  });
}

export function usePodDetail(
  clusterId: string,
  ns: string,
  name: string,
  uid: string,
  range: RangeKey,
  refreshMs: number,
) {
  return useQuery({
    ...common,
    queryKey: drillKeys.pod(clusterId, ns, name, uid, range),
    queryFn: ({ signal }) =>
      apiGet<PodDetailResponse>(
        `/api/v1/clusters/${encodeURIComponent(clusterId)}/pods/${encodeURIComponent(name)}`,
        { ns, range, ...(uid ? { uid } : {}) },
        signal,
      ),
    refetchInterval: refreshMs > 0 ? refreshMs : false,
    enabled: Boolean(ns && name),
  });
}


/* ══════════════════════════════════════════════════════════════════════════
   Logs Explorer (이슈 #16)
   ══════════════════════════════════════════════════════════════════════════ */

export interface LogFilters {
  clusterId: string;
  namespace: string;
  workload: string;
  podUid: string;
  container: string;
  levels: string[];
  q: string;
  range: RangeKey;
  /** 차트 구간 선택으로 좁힌 범위. 없으면 range를 씁니다. */
  from?: number;
  to?: number;
}

export const logKeys = {
  search: (f: LogFilters) =>
    [
      "logs",
      f.clusterId,
      f.namespace,
      f.workload,
      f.podUid,
      f.container,
      [...f.levels].sort().join(","),
      f.q,
      f.range,
      f.from ?? null,
      f.to ?? null,
    ] as const,
};

/**
 * 무한 스크롤. **offset이 아니라 cursor**를 씁니다.
 * 로그가 계속 들어오는 동안 offset을 쓰면 페이지 경계가 밀려 중복·누락이 생깁니다.
 * (이슈 #16 완료 기준)
 */
export function useLogSearch(f: LogFilters) {
  return useInfiniteQuery({
    queryKey: logKeys.search(f),
    initialPageParam: "" as string,
    queryFn: ({ signal, pageParam }) =>
      apiGet<LogSearchResponse>(
        `/api/v1/clusters/${encodeURIComponent(f.clusterId)}/logs`,
        {
          range: f.range,
          ...(f.namespace && f.namespace !== "all" ? { ns: f.namespace } : {}),
          ...(f.workload ? { workload: f.workload } : {}),
          ...(f.podUid ? { podUid: f.podUid } : {}),
          ...(f.container ? { container: f.container } : {}),
          ...(f.levels.length ? { levels: f.levels.join(",") } : {}),
          ...(f.q ? { q: f.q } : {}),
          ...(f.from ? { from: String(f.from) } : {}),
          ...(f.to ? { to: String(f.to) } : {}),
          ...(pageParam ? { cursor: pageParam } : {}),
        },
        signal,
      ),
    getNextPageParam: (last) => last.cursor.next ?? undefined,
    refetchOnWindowFocus: false,
    retry: retryPolicy,
    staleTime: 10_000,
  });
}


/* ══════════════════════════════════════════════════════════════════════════
   Pod Topology · Alerts
   ══════════════════════════════════════════════════════════════════════════ */

export const topoKeys = {
  graph: (clusterId: string, ns: string, range: RangeKey) => ["topology", clusterId, ns, range] as const,
  series: (clusterId: string, edgeId: string, range: RangeKey) =>
    ["topology-series", clusterId, edgeId, range] as const,
};

export function useTopology(clusterId: string, ns: string, range: RangeKey, refreshMs: number) {
  return useQuery({
    ...common,
    queryKey: topoKeys.graph(clusterId, ns, range),
    queryFn: ({ signal }) =>
      apiGet<TopologyResponse>(
        `/api/v1/clusters/${encodeURIComponent(clusterId)}/topology`,
        { range, ...(ns && ns !== "all" ? { ns } : {}) },
        signal,
      ),
    refetchInterval: refreshMs > 0 ? refreshMs : false,
  });
}

/**
 * 공유 배치 저장 (관리자 전용). 성공하면 topology 화면을 다시 조회해
 * 모든 패널이 같은 저장본을 보게 합니다. 빈 positions는 기본 배치로 초기화합니다. (#28)
 */
export function useSaveTopologyLayout(clusterId: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (positions: TopologyNodePosition[]) =>
      apiRequest<TopologyLayout>(`/api/v1/clusters/${encodeURIComponent(clusterId)}/topology/layout`, {
        method: "PUT",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ positions }),
      }),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["topology", clusterId] }),
  });
}

/**
 * 엣지 시계열은 **선택했을 때만** 조회합니다.
 * 그래프를 그릴 때 모든 엣지의 시계열을 미리 받으면 화면 하나에 12번의 조회가 붙습니다.
 * 화면 단위 집계(ADR 0002)의 예외이며, 사용자 조작으로 발생하는 추가 조회입니다.
 */
export function useEdgeSeries(clusterId: string, edgeId: string | null, range: RangeKey) {
  return useQuery({
    ...common,
    queryKey: topoKeys.series(clusterId, edgeId ?? "", range),
    queryFn: ({ signal }) =>
      apiGet<TopologyEdgeSeriesResponse>(
        `/api/v1/clusters/${encodeURIComponent(clusterId)}/topology/edges/${encodeURIComponent(edgeId!)}/series`,
        { range },
        signal,
      ),
    enabled: Boolean(edgeId),
  });
}

export const alertKeys = {
  list: (clusterId: string, ns: string, range: RangeKey) => ["alerts", clusterId, ns, range] as const,
};

export function useAlerts(clusterId: string, ns: string, range: RangeKey, refreshMs: number) {
  return useQuery({
    ...common,
    queryKey: alertKeys.list(clusterId, ns, range),
    queryFn: ({ signal }) =>
      apiGet<AlertListResponse>(
        `/api/v1/clusters/${encodeURIComponent(clusterId)}/alerts`,
        { range, ...(ns && ns !== "all" ? { ns } : {}) },
        signal,
      ),
    refetchInterval: refreshMs > 0 ? refreshMs : false,
  });
}

/* ══════════════════════════════════════════════════════════════════════════
   Resource Explorer (ADR 0018)
   --------------------------------------------------------------------------
   조회 전용입니다. 브라우저는 Kubernetes를 직접 부르지 않고 BFF의 catalog/list/
   detail 세 경로만 씁니다. 목록은 서버 cursor로만 이어보고(offset 없음),
   **폴링하지 않습니다** — 갱신은 사용자가 명시적으로 일으킵니다.
   ══════════════════════════════════════════════════════════════════════════ */

export const resourceKeys = {
  catalog: (clusterId: string) => ["resource-catalog", clusterId] as const,
  list: (clusterId: string, gvr: string, ns: string, namePrefix: string, labelSelector: string, order: string) =>
    ["resource-list", clusterId, gvr, ns, namePrefix, labelSelector, order] as const,
  object: (clusterId: string, gvr: string, ns: string, name: string, uid: string) =>
    ["resource-object", clusterId, gvr, ns, name, uid] as const,
};

/* 상태(unsupported·forbidden·syncing·unavailable)는 재시도로 바뀌지 않습니다.
   재시도하면 되지 않을 요청을 반복해 BFF와 클러스터에 부하만 더합니다. */
const resourceRetry = (count: number, error: unknown) =>
  !(error instanceof HttpError && error.status >= 400 && error.status !== 500) && count < 2;

export function useResourceCatalog(clusterId: string, enabled: boolean) {
  return useQuery({
    queryKey: resourceKeys.catalog(clusterId),
    queryFn: ({ signal }) =>
      apiGet<ResourceCatalogResponse>(`/api/v1/clusters/${encodeURIComponent(clusterId)}/resources`, {}, signal),
    enabled: enabled && Boolean(clusterId),
    refetchOnWindowFocus: false,
    staleTime: 60_000,
    retry: resourceRetry,
  });
}

export type ResourceListFilters = {
  clusterId: string;
  group: string;
  version: string;
  resource: string;
  /** "all"이면 서버가 Scope 전체를 적용합니다. */
  namespace: string;
  namePrefix: string;
  labelSelector: string;
  order: "asc" | "desc";
  limit: number;
};

function resourcePath(f: { clusterId: string; group: string; version: string; resource: string }) {
  return `/api/v1/clusters/${encodeURIComponent(f.clusterId)}/resources/${encodeURIComponent(f.group)}/${encodeURIComponent(f.version)}/${encodeURIComponent(f.resource)}`;
}

/**
 * 서버 keyset cursor로만 이어봅니다. offset 페이징은 만들지 않습니다 (ADR 0003).
 * cursor는 서버가 만든 불투명 문자열이며 클라이언트는 해석하지 않습니다.
 */
export function useResourceList(f: ResourceListFilters, enabled: boolean) {
  return useInfiniteQuery({
    queryKey: resourceKeys.list(
      f.clusterId,
      `${f.group}/${f.version}/${f.resource}`,
      f.namespace,
      f.namePrefix,
      f.labelSelector,
      f.order,
    ),
    initialPageParam: "" as string,
    queryFn: ({ signal, pageParam }) =>
      apiGet<ResourceListResponse>(
        resourcePath(f),
        {
          limit: String(f.limit),
          ...(f.namespace && f.namespace !== "all" ? { ns: f.namespace } : {}),
          ...(f.namePrefix ? { name: f.namePrefix } : {}),
          ...(f.labelSelector ? { labelSelector: f.labelSelector } : {}),
          ...(f.order === "desc" ? { order: "desc" } : {}),
          ...(pageParam ? { cursor: pageParam } : {}),
        },
        signal,
      ),
    getNextPageParam: (last) => last.nextCursor || undefined,
    enabled: enabled && Boolean(f.clusterId) && Boolean(f.resource),
    refetchOnWindowFocus: false,
    retry: resourceRetry,
    staleTime: 10_000,
  });
}

/**
 * 상세는 **사용자가 항목을 연 순간에만** 조회합니다. 서버가 격리된 client로
 * live GET하고 정제한 YAML만 돌려줍니다 — 값이 아니라 메타만 옵니다.
 */
export function useResourceObject(
  clusterId: string,
  gvr: { group: string; version: string; resource: string },
  target: { namespace: string; name: string; uid: string } | null,
) {
  return useQuery({
    queryKey: resourceKeys.object(
      clusterId,
      `${gvr.group}/${gvr.version}/${gvr.resource}`,
      target?.namespace ?? "",
      target?.name ?? "",
      target?.uid ?? "",
    ),
    queryFn: ({ signal }) =>
      apiGet<ResourceDetailResponse>(
        `${resourcePath({ clusterId, ...gvr })}/object`,
        {
          ...(target?.namespace ? { namespace: target.namespace } : {}),
          name: target?.name ?? "",
          uid: target?.uid ?? "",
        },
        signal,
      ),
    enabled: Boolean(clusterId) && Boolean(gvr.resource) && Boolean(target?.name) && Boolean(target?.uid),
    refetchOnWindowFocus: false,
    staleTime: 30_000,
    retry: resourceRetry,
  });
}
