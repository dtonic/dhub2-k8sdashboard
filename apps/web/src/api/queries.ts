import { keepPreviousData, useQuery } from "@tanstack/react-query";
import type { ClusterOverviewResponse, RangeKey, ScopeResponse } from "@k8s-dashboard/contracts";
import { apiGet } from "./client";

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
    retry: (count, error) => !(error instanceof Error && error.name === "HttpError") && count < 2,
    staleTime: 10_000,
  });
}
