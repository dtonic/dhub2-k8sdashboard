import { useCallback, useEffect, useMemo } from "react";
import { useParams, useSearchParams } from "react-router-dom";
import type { RangeKey } from "@k8s-dashboard/contracts";
import { useDashboardStore } from "./dashboardStore";

const RANGES: RangeKey[] = ["30d", "7d", "1d", "1h", "custom"];
export const REFRESH_OPTIONS = [
  { value: 0, label: "수동" },
  { value: 10_000, label: "10초" },
  { value: 30_000, label: "30초" },
  { value: 60_000, label: "1분" },
];

/**
 * Scope · 시간 범위 · 자동 갱신 주기를 **URL 쿼리**에 둡니다.
 * 링크 하나로 같은 화면을 재현할 수 있어야 장애 대응 중 공유가 됩니다.
 */
export function useDashboardParams() {
  const [params, setParams] = useSearchParams();

  /* 기본 클러스터는 하드코딩하지 않습니다 — cluster 파라미터가 없으면 AppShell이
     /api/v1/scope의 첫 접근 가능 클러스터를 URL에 채울 때까지 화면을 열지 않습니다. (#27) */
  const clusterId = params.get("cluster") ?? "";
  const namespace = params.get("ns") ?? "all";
  const rangeParam = params.get("range") as RangeKey | null;
  const range: RangeKey = rangeParam && RANGES.includes(rangeParam) ? rangeParam : "1h";
  const refreshMs = Number(params.get("refresh") ?? 30_000);
  const safeRefreshMs = Number.isFinite(refreshMs) ? refreshMs : 30_000;
  const pathParams = useParams();

  /* zustand 스토어를 URL과 동기화합니다 — URL(query/path parameter)이 진실 원천이고,
     값이 어긋나면 항상 URL 값으로 스토어를 덮어씁니다. (#28 후속 요구) */
  const pathKey = JSON.stringify(pathParams);
  useEffect(() => {
    useDashboardStore.getState().syncFromUrl({
      clusterId,
      namespace,
      range,
      refreshMs: safeRefreshMs,
      pathParams,
    });
    // eslint-disable-next-line react-hooks/exhaustive-deps -- pathParams는 pathKey로 비교합니다.
  }, [clusterId, namespace, range, safeRefreshMs, pathKey]);

  const patch = useCallback(
    (next: Partial<{ cluster: string; ns: string; range: RangeKey; refresh: number }>) => {
      setParams(
        (prev) => {
          const p = new URLSearchParams(prev);
          if (next.cluster !== undefined) {
            p.set("cluster", next.cluster);
            /* 클러스터를 바꾸면 namespace 선택은 유효하지 않을 수 있으므로 초기화합니다. */
            p.set("ns", "all");
          }
          if (next.ns !== undefined) p.set("ns", next.ns);
          if (next.range !== undefined) p.set("range", next.range);
          if (next.refresh !== undefined) p.set("refresh", String(next.refresh));
          return p;
        },
        { replace: true },
      );
    },
    [setParams],
  );

  return useMemo(
    () => ({
      clusterId,
      namespace,
      range,
      refreshMs: safeRefreshMs,
      patch,
    }),
    [clusterId, namespace, range, safeRefreshMs, patch],
  );
}
