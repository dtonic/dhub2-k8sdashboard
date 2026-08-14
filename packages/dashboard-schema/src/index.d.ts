export const DASHBOARD_LIMITS: Readonly<{
  files: 32; fileBytes: 65536; widgets: 24; variables: 2; string: 160; columns: 12; rows: 96;
}>;
export type WidgetType = "TimeSeries" | "Stat" | "Gauge" | "Table" | "LogStream" | "EventTimeline";
export type AggregateBinding =
  | "trends" | "nodes.ready" | "pods.running" | "pods.runningPercent"
  | "unhealthy" | "events" | "unsupported.logs";
export interface DashboardVariable { id: string; label: string; kind: "scope" | "range"; }
export interface DashboardLayout { x: number; y: number; w: number; h: number; }
interface WidgetBase { id: string; title: string; layout: DashboardLayout; }
export interface TimeSeriesWidget extends WidgetBase { type: "TimeSeries"; binding: "trends"; queryRefs: string[]; }
export interface StatWidget extends WidgetBase { type: "Stat"; binding: "nodes.ready" | "pods.running"; }
export interface GaugeWidget extends WidgetBase { type: "Gauge"; binding: "pods.runningPercent"; }
export interface TableWidget extends WidgetBase { type: "Table"; binding: "unhealthy"; options?: { maxRows: number }; }
export interface LogStreamWidget extends WidgetBase { type: "LogStream"; binding: "unsupported.logs"; }
export interface EventTimelineWidget extends WidgetBase { type: "EventTimeline"; binding: "events"; options?: { maxRows: number }; }
export type DashboardWidget = TimeSeriesWidget | StatWidget | GaugeWidget | TableWidget | LogStreamWidget | EventTimelineWidget;
export interface DashboardDefinition { schemaVersion: 1; id: string; title: string; description?: string; variables: DashboardVariable[]; widgets: DashboardWidget[]; }
export interface ValidationResult { valid: boolean; errors: string[]; }
export interface EmbeddedFile { name: string; kind: "file" | "directory" | "symlink" | "other"; size: number; text?: string; }
export function validateDashboard(value: unknown, queryRefs?: ReadonlySet<string>): ValidationResult;
export function dashboardControlKinds(definition: DashboardDefinition): Array<DashboardVariable["kind"]>;
export function migrateDashboard(value: unknown): DashboardDefinition;
export function validateEmbeddedFiles(files: readonly EmbeddedFile[], queryRefs: ReadonlySet<string>): DashboardDefinition[];
