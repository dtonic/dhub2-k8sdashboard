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
  ResourceRecentItem,
  ResourceRecentResponse,
  ResourceSearchResponse,
  ScopeResponse,
  TopologyEdgeSeriesResponse,
  TopologyLayout,
  TopologyNodePosition,
  TopologyResponse,
  WorkloadDetailResponse,
} from "@k8s-dashboard/contracts";
import { chunkRecentRefs, REF_KEY, type RecentRef } from "@/app/recent";
import { apiGet, apiRequest, currentScenarioParam, HttpError } from "./client";

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

/* ══════════════════════════════════════════════════════════════════════════
   전역 리소스 검색 · 최근 항목 (ADR 0023)
   --------------------------------------------------------------------------
   Command Palette의 유일한 데이터 원천입니다. Kubernetes를 직접 부르지 않고
   BFF의 search/recent 두 경로만 씁니다. **폴링하지 않습니다** — 사용자가 입력할
   때만, 그것도 디바운스 뒤에 한 번 나갑니다.
   ══════════════════════════════════════════════════════════════════════════ */

/** 서버가 강제하는 질의 길이입니다. UI도 같은 값을 씁니다. */
export const SEARCH_MIN_QUERY = 2;
export const SEARCH_MAX_QUERY = 64;
/** 서버 MaxSearchPageSize와 같은 값입니다. 더 큰 값을 보내면 400입니다. */
export const SEARCH_MAX_RESULTS = 50;

export const searchKeys = {
  search: (clusterId: string, query: string, cursor: string) =>
    ["resource-search", clusterId, query, cursor] as const,
  /**
   * `epoch`는 팔레트를 여는 횟수입니다. 열 때마다 키가 달라지므로 **반드시 다시
   * 물어보고**, 답이 오기 전까지 이전 열기의 캐시된 제목이 렌더되지 않습니다.
   */
  recent: (clusterId: string, refs: string, epoch = 0) => ["resource-recent", clusterId, refs, epoch] as const,
};

/**
 * 입력을 질의어로 정규화합니다.
 *
 * 화면(raw)과 디바운스된 값이 **같은 규칙**으로 정규화되어야 "지금 화면의 입력에
 * 대응하는 결과인가"를 문자열 비교 하나로 판단할 수 있습니다. 한쪽만 trim하면
 * `"pay "`와 `"pay"`가 다른 질의로 보여 낡은 결과가 최신처럼 렌더됩니다.
 */
export function normalizeSearchQuery(raw: string): string {
  return raw.trim().slice(0, SEARCH_MAX_QUERY);
}

/** 질의 자체가 상한을 벗어났는지. 요청을 만들기 전에 UI가 먼저 봅니다. */
export function searchQueryUsable(raw: string): boolean {
  const q = normalizeSearchQuery(raw);
  return q.length >= SEARCH_MIN_QUERY && q.length <= SEARCH_MAX_QUERY;
}

/** 최근 항목 캐시 키의 참조 부분. 훅과 취소 경로가 같은 문자열을 봐야 합니다. */
export function recentRefsKey(refs: readonly RecentRef[]): string {
  return refs.map((r) => `${r.clusterId} ${r.group}/${r.version}/${r.resource}/${r.uid}`).join(",");
}

/** 최근 항목 요청의 경로. 청킹이 실제 request target을 재려면 이 값이 필요합니다. */
export function recentPath(clusterId: string): string {
  return `/api/v1/clusters/${encodeURIComponent(clusterId)}/resources/recent`;
}

/**
 * 전역 검색. **namespace 파라미터가 없는 것이 계약입니다** — 범위는 언제나
 * "이 사용자가 볼 수 있는 전부"이고 UI가 넓히거나 좁혀 떠볼 수 없습니다.
 *
 * 취소는 TanStack이 주는 signal을 그대로 넘겨 네트워크까지 전파합니다. 질의어가
 * 바뀌면 queryKey가 바뀌므로 **늦게 도착한 옛 응답이 새 결과를 덮어쓸 수 없습니다.**
 */
export function useResourceSearch(clusterId: string, query: string, enabled: boolean, cursor = "") {
  const q = normalizeSearchQuery(query);
  return useQuery({
    queryKey: searchKeys.search(clusterId, q, cursor),
    queryFn: ({ signal }) =>
      apiGet<ResourceSearchResponse>(
        `/api/v1/clusters/${encodeURIComponent(clusterId)}/resources/search`,
        { q, limit: String(SEARCH_MAX_RESULTS), ...(cursor ? { cursor } : {}) },
        signal,
      ),
    enabled: enabled && Boolean(clusterId) && searchQueryUsable(q),
    refetchOnWindowFocus: false,
    /* 폴링하지 않습니다. 같은 질의어를 다시 열면 짧은 시간 안에는 캐시를 씁니다. */
    staleTime: 15_000,
    retry: resourceRetry,
  });
}

/**
 * 최근 항목 재해석. 제목의 근거는 전부 서버 값입니다.
 *
 * 참조가 많으면 **요청을 나눠** 보냅니다(query string 6KiB 상한). 나눈 순서가
 * 곧 원래 순서이므로 응답을 순서대로 이어 붙이면 브라우저의 최신순이 그대로
 * 보존됩니다. 취소 신호는 **하나를 공유**해 팔레트를 닫으면 남은 요청도 함께 끊깁니다.
 */
export async function fetchRecentResources(
  clusterId: string,
  refs: readonly RecentRef[],
  signal?: AbortSignal,
): Promise<ResourceRecentItem[]> {
  const pathname = recentPath(clusterId);
  /* 청킹이 실패하면 **요청을 하나도 만들지 않습니다.** 상한을 넘는 요청을 쏘아
     414를 받는 것보다 왜 못 보냈는지 오류로 말하는 편이 정직합니다. */
  const chunks = chunkRecentRefs(refs, { pathname, extraParams: currentScenarioParam() });
  /* 참조가 없어도 **한 번은 물어봅니다.** 0건 조회는 계약이 지원하는 형태이고,
     그 응답이 "최근이 비었다"와 "검색이 꺼졌다"를 구분해 줍니다. 안 물어보면
     기능이 없는 배포에서도 그냥 빈 목록으로 보여 사용자가 원인을 오판합니다.
     이 0건 target도 위 청킹이 이미 같은 자로 재고 통과시킨 것입니다. */
  if (chunks.length === 0) chunks.push([]);

  const out: ResourceRecentItem[] = [];
  for (const chunk of chunks) {
    /* 신호는 **모든 덩어리가 공유**합니다 — 팔레트를 닫으면 남은 덩어리도 함께 끊깁니다. */
    if (signal?.aborted) throw signal.reason;
    /* 같은 이름의 파라미터가 반복되므로 apiGet의 Record 시그니처를 쓸 수 없습니다.
       여기서 만드는 문자열이 곧 청킹이 잰 대상입니다. */
    const search = chunk.map((ref) => `${REF_KEY}=${ref}`).join("&");
    const page = await apiGet<ResourceRecentResponse>(
      search ? `${pathname}?${search}` : pathname,
      {},
      signal,
    );
    out.push(...page.items);
  }
  return out;
}

/**
 * 열 때마다 **다시 물어봅니다.** 캐시된 제목을 그대로 보여주면 권한이 사라졌거나
 * 교체된 항목이 옛 제목으로 남습니다. 그래서 `staleTime: 0`이고, 재확인이 도는
 * 동안에는 팔레트가 이전 값을 렌더하지 않습니다(`isFetching`을 봅니다).
 *
 * `epoch`가 키에 들어가므로 다시 열면 이전 열기의 데이터가 캐시로 살아나지 않습니다.
 * 호출부는 **저장소를 다 읽은 뒤에만** `enabled`를 켜야 합니다 — 아직 읽지 않은
 * 빈 목록으로 켜면 0건 probe가 헛나가고 곧 두 번째 질의가 따라붙습니다.
 */
export function useRecentResources(
  clusterId: string,
  refs: readonly RecentRef[],
  enabled: boolean,
  epoch = 0,
) {
  return useQuery({
    /* 참조 목록과 열기 회차가 캐시 키입니다 — 목록이 바뀌거나 다시 열면 다시 해석합니다. */
    queryKey: searchKeys.recent(clusterId, recentRefsKey(refs), epoch),
    queryFn: ({ signal }) => fetchRecentResources(clusterId, refs, signal),
    /* 참조 0건에서도 켭니다. 0건 probe가 기능 가용성을 알려주는 유일한 신호입니다. */
    enabled: enabled && Boolean(clusterId),
    refetchOnWindowFocus: false,
    staleTime: 0,
    gcTime: 0,
    retry: resourceRetry,
  });
}
