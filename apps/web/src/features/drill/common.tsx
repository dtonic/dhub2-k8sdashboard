import { useCallback, type ReactNode } from "react";
import { useQueryClient } from "@tanstack/react-query";
import { useSearchParams } from "react-router-dom";
import type { IssueReason, RangeKey } from "@k8s-dashboard/contracts";
import { HttpError } from "@/api/client";
import { useScope } from "@/api/queries";
import { useDashboardParams } from "@/state/useDashboardParams";
import { NAMESPACE_NAMES } from "@/mocks/drilldown";
import { RefreshControl, ScopeSelector, TimeRangePicker } from "@/components/controls";
import { ErrorState, ForbiddenState } from "@/components/SectionState";

/** 드릴다운 화면들이 공유하는 컨트롤 묶음. Scope·범위·갱신 주기는 URL에 남습니다. */
export function useDrillControls(invalidate: () => void, fetching: boolean, updatedAtMs?: number) {
  const { clusterId, namespace, range, refreshMs, patch } = useDashboardParams();
  const scope = useScope();

  const controls = (
    <div className="row row--wrap">
      <ScopeSelector
        scope={scope.data}
        clusterId={clusterId}
        namespace={namespace}
        namespaces={NAMESPACE_NAMES}
        onChange={(next) => patch(next)}
        disabled={scope.isLoading}
      />
      <TimeRangePicker range={range} onChange={(r) => patch({ range: r })} />
      <RefreshControl
        refreshMs={refreshMs}
        onChange={(ms) => patch({ refresh: ms })}
        onRefreshNow={invalidate}
        fetching={fetching}
        updatedAtMs={updatedAtMs}
      />
    </div>
  );

  return { clusterId, namespace, range: range as RangeKey, refreshMs, patch, controls };
}

/**
 * 상태 필터 선택을 URL에 둡니다.
 * 자동 갱신이나 범위 변경으로 데이터가 바뀌어도 선택이 초기화되지 않아야 합니다.
 * (이슈 #15 완료 기준)
 */
export function useIssueFilter() {
  const [params, setParams] = useSearchParams();
  const selected = (params.get("issues") ?? "").split(",").filter(Boolean) as IssueReason[];

  const toggle = useCallback(
    (r: IssueReason) => {
      setParams(
        (prev) => {
          const p = new URLSearchParams(prev);
          const cur = (p.get("issues") ?? "").split(",").filter(Boolean);
          const next = cur.includes(r) ? cur.filter((x) => x !== r) : [...cur, r];
          if (next.length) p.set("issues", next.join(","));
          else p.delete("issues");
          return p;
        },
        { replace: true },
      );
    },
    [setParams],
  );

  const clear = useCallback(() => {
    setParams(
      (prev) => {
        const p = new URLSearchParams(prev);
        p.delete("issues");
        return p;
      },
      { replace: true },
    );
  }, [setParams]);

  return { selected, toggle, clear };
}

/** 화면 전체 실패(403/404/기타)를 공통으로 그립니다. 섹션 단위 실패와 구분됩니다. */
export function PageError({ error, onRetry }: { error: unknown; onRetry: () => void }) {
  if (error instanceof HttpError && error.status === 403) {
    return (
      <section className="panel">
        <div className="panel__body">
          <ForbiddenState detail={error.body.message} />
        </div>
      </section>
    );
  }
  return (
    <section className="panel">
      <div className="panel__body">
        <ErrorState detail={error instanceof Error ? error.message : undefined} onRetry={onRetry} />
      </div>
    </section>
  );
}

export function useInvalidate(key: readonly unknown[]) {
  const qc = useQueryClient();
  return useCallback(() => void qc.invalidateQueries({ queryKey: key }), [qc, key]);
}

export function PageHeader({
  title,
  subtitle,
  controls,
  crumbs,
  actions,
}: {
  title: ReactNode;
  subtitle?: ReactNode;
  controls?: ReactNode;
  crumbs?: ReactNode;
  actions?: ReactNode;
}) {
  return (
    <>
      {crumbs}
      <header className="page__header">
        <div>
          <h1 className="page__title">{title}</h1>
          {subtitle && <p className="page__subtitle">{subtitle}</p>}
          {actions && (
            <div className="row row--wrap" style={{ marginTop: "var(--space-4)" }}>
              {actions}
            </div>
          )}
        </div>
        {controls}
      </header>
    </>
  );
}
