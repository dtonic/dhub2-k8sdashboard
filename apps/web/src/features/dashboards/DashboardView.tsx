import { useParams } from "react-router-dom";
import { useQueryClient } from "@tanstack/react-query";
import { RANGE_LABEL } from "@k8s-dashboard/contracts";
import { dashboardControlKinds, validateDashboard } from "@k8s-dashboard/dashboard-schema";
import type { DashboardDefinition } from "@k8s-dashboard/dashboard-schema";
import { queryKeys, useClusterOverview, useScope } from "@/api/queries";
import { ErrorState, ForbiddenState } from "@/components/SectionState";
import { RefreshControl, ScopeSelector, TimeRangePicker } from "@/components/controls";
import { embeddedDashboards } from "@/generated/dashboards";
import { useDashboardParams } from "@/state/useDashboardParams";
import { HttpError } from "@/api/client";
import { DashboardWidgetView } from "./WidgetRegistry";

export function DashboardView() {
  const { id } = useParams();
  const definition = embeddedDashboards.find((dashboard) => dashboard.id === id);
  const validation = validateDashboard(definition);
  if (!definition || !validation.valid) return <div className="page"><ErrorState detail="Dashboard definition is missing or invalid." /></div>;
  return <ResolvedDashboard definition={definition} />;
}

export function ResolvedDashboard({ definition }: { definition: DashboardDefinition }) {
  const { clusterId, namespace, range, refreshMs, patch } = useDashboardParams();
  const scope = useScope();
  const queryClient = useQueryClient();
  const overview = useClusterOverview({ clusterId, namespace, range, refreshMs });

  const forbidden = overview.error instanceof HttpError && overview.error.status === 403;
  return <div className="page">
    <header className="page__header">
      <div><h1 className="page__title">{definition.title}</h1><p className="page__subtitle">{definition.description ? `${definition.description} · ` : ""}{RANGE_LABEL[range]}</p></div>
      <div className="row row--wrap">
        {dashboardControlKinds(definition).map((kind) => kind === "scope"
          ? <ScopeSelector key={kind} scope={scope.data} clusterId={clusterId} namespace={namespace} onChange={patch} disabled={scope.isLoading} />
          : <TimeRangePicker key={kind} range={range} onChange={(next) => patch({ range: next })} />)}
        <RefreshControl refreshMs={refreshMs} onChange={(next) => patch({ refresh: next })}
          onRefreshNow={() => queryClient.invalidateQueries({ queryKey: queryKeys.overview(clusterId, namespace, range) })}
          fetching={overview.isFetching} updatedAtMs={overview.dataUpdatedAt || undefined} />
      </div>
    </header>
    {forbidden ? <section className="panel"><div className="panel__body"><ForbiddenState detail={(overview.error as HttpError).body.message} /></div></section>
      : overview.isError ? <section className="panel"><div className="panel__body"><ErrorState detail={(overview.error as Error).message} /></div></section>
      : <div className="dashboard-grid">{definition.widgets.map((widget) => <div key={widget.id} style={{ gridColumn: `${widget.layout.x + 1} / span ${widget.layout.w}`, gridRow: `${widget.layout.y + 1} / span ${widget.layout.h}` }}><DashboardWidgetView widget={widget} overview={overview.data} loading={overview.isLoading} /></div>)}</div>}
  </div>;
}
