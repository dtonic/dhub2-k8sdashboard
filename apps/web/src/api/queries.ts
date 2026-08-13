import { keepPreviousData, useQuery } from "@tanstack/react-query";
import type {
  ClusterOverviewResponse,
  NamespaceDetailResponse,
  NamespaceListResponse,
  PodDetailResponse,
  RangeKey,
  ScopeResponse,
  WorkloadDetailResponse,
} from "@k8s-dashboard/contracts";
import { apiGet, HttpError } from "./client";

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
  namespaces: (clusterId: string, range: RangeKey) => ["namespaces", clusterId, range] as const,
  namespace: (clusterId: string, ns: string, range: RangeKey) => ["namespace", clusterId, ns, range] as const,
  workload: (clusterId: string, ns: string, kind: string, name: string, range: RangeKey) =>
    ["workload", clusterId, ns, kind, name, range] as const,
  /** Pod는 UID가 신원입니다. 이름이 같아도 재생성되면 다른 캐시 항목입니다. */
  pod: (clusterId: string, ns: string, name: string, uid: string, range: RangeKey) =>
    ["pod", clusterId, ns, name, uid, range] as const,
};

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
