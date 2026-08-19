import type { ComponentType } from "react";
import type { ClusterOverviewResponse, Section, TrendPanel } from "@k8s-dashboard/contracts";
import type { DashboardWidget, WidgetType } from "@k8s-dashboard/dashboard-schema";
import { LineChart } from "@/components/LineChart";
import { VirtualTable } from "@/components/VirtualTable";
import { ErrorState, SectionView } from "@/components/SectionState";
import { Panel, StatTile, StatusBadge } from "@/components/primitives";
import { embeddedDashboardBindings } from "@/generated/dashboards";

export interface WidgetProps { widget: DashboardWidget; overview?: ClusterOverviewResponse; loading: boolean; }

function state<T>(section: Section<T> | undefined, loading: boolean, empty: string, render: (data: T) => React.ReactNode) {
  return <SectionView section={section} loading={loading} emptyTitle={empty}>{(data) => render(data)}</SectionView>;
}

function TimeSeries({ widget, overview, loading }: WidgetProps) {
  if (widget.type !== "TimeSeries") return <ErrorState detail="Widget contract mismatch" />;
  return state(overview?.trends, loading, "No time-series data", (panels: TrendPanel[]) => {
    const refs = new Set(widget.queryRefs);
    const selected = embeddedDashboardBindings.filter((binding) => refs.has(binding.queryRef)).flatMap((binding) => {
      const panel = panels.find((candidate) => candidate.id === binding.panelId);
      const series = panel?.series.find((candidate) => candidate.key === binding.seriesKey);
      return panel && series ? [{ panel, series }] : [];
    });
    if (selected.length !== refs.size) return <ErrorState detail="The dashboard queryRef is not present in the overview response." />;
    const units = new Set(selected.map(({ series }) => series.unit));
    if (units.size !== 1) return <ErrorState detail="A chart cannot combine different units." />;
    return <LineChart series={selected.map(({ series }) => series)} stepSeconds={selected[0]!.panel.stepSeconds} ariaLabel={widget.title} />;
  });
}

function Stat({ widget, overview, loading }: WidgetProps) {
  if (widget.type !== "Stat") return <ErrorState detail="Widget contract mismatch" />;
  if (widget.binding === "nodes.ready") return state(overview?.nodes, loading, "No node statistic available", (nodes) =>
    <StatTile label={widget.title} value={`${nodes.ready}/${nodes.total}`} />);
  return state(overview?.pods, loading, "No pod statistic available", (pods) =>
    <StatTile label={widget.title} value={pods.running.toLocaleString()} />);
}

function Gauge({ widget, overview, loading }: WidgetProps) {
  if (widget.type !== "Gauge") return <ErrorState detail="Widget contract mismatch" />;
  return state(overview?.pods, loading, "No pod health data", (pods) => {
    const percent = pods.total === 0 ? 0 : Math.round((pods.running / pods.total) * 100);
    return <div className="dashboard-gauge" role="meter" aria-label={widget.title} aria-valuemin={0} aria-valuemax={100} aria-valuenow={percent}>
      <strong>{percent}%</strong><span>{pods.running.toLocaleString()} of {pods.total.toLocaleString()} running</span>
    </div>;
  });
}

function Table({ widget, overview, loading }: WidgetProps) {
  if (widget.type !== "Table") return <ErrorState detail="Widget contract mismatch" />;
  return state(overview?.unhealthy, loading, "No unhealthy workloads", (rows) => <VirtualTable
    items={rows.slice(0, widget.options?.maxRows ?? 500)} height={260}
    columns={["40%", "24%", "24%", "12%"]}
    header={<tr><th>Name</th><th>Namespace</th><th>Status</th><th className="ds-num">Restarts</th></tr>}
    getKey={(row) => `${row.kind}-${row.ref.podUid ?? row.ref.workloadUid ?? row.name}`}
    renderRow={(row) => <><td className="ds-ident">{row.name}</td><td>{row.namespace}</td><td><StatusBadge severity={row.severity} label={row.reason} small /></td><td className="ds-num">{row.restarts}</td></>}
  />);
}

function LogStream({ widget }: WidgetProps) {
  if (widget.type !== "LogStream") return <ErrorState detail="Widget contract mismatch" />;
  return <ErrorState detail="LogStream is unavailable in the overview aggregate. Open Logs Explorer for an explicit search." />;
}

function EventTimeline({ widget, overview, loading }: WidgetProps) {
  if (widget.type !== "EventTimeline") return <ErrorState detail="Widget contract mismatch" />;
  return state(overview?.events, loading, "No recent events", (events) => <VirtualTable
    items={events.slice(0, widget.options?.maxRows ?? 200)} height={260}
    columns={["16%", "28%", "44%", "12%"]}
    header={<tr><th>Type</th><th>Reason</th><th>Object</th><th className="ds-num">Count</th></tr>}
    getKey={(event) => event.id}
    renderRow={(event) => <><td>{event.type}</td><td>{event.reason}</td><td className="ds-ident">{event.involvedName}</td><td className="ds-num">{event.count}</td></>}
  />);
}

export const WIDGET_REGISTRY = {
  TimeSeries, Stat, Gauge, Table, LogStream, EventTimeline,
} satisfies Record<WidgetType, ComponentType<WidgetProps>>;

export function DashboardWidgetView(props: WidgetProps) {
  const Component = (WIDGET_REGISTRY as Record<string, ComponentType<WidgetProps> | undefined>)[props.widget.type];
  if (!Component) return <Panel title="Invalid widget"><ErrorState detail={`Unknown widget type: ${String(props.widget.type)}`} /></Panel>;
  return <Panel title={props.widget.title} section={bindingSection(props.widget, props.overview)} referenceIso={props.overview?.generatedAt}><Component {...props} /></Panel>;
}

function bindingSection(widget: DashboardWidget, overview?: ClusterOverviewResponse): Section<unknown> | undefined {
  switch (widget.binding) {
    case "trends": return overview?.trends;
    case "nodes.ready": return overview?.nodes;
    case "pods.running": case "pods.runningPercent": return overview?.pods;
    case "unhealthy": return overview?.unhealthy;
    case "events": return overview?.events;
    case "unsupported.logs": return undefined;
    default: return undefined;
  }
}
