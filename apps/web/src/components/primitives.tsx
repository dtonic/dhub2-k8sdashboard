import type { ReactNode } from "react";
import type { Section, Severity } from "@k8s-dashboard/contracts";
import { since } from "@/lib/format";
import { sourceLabel } from "./SectionState";

/* ── Status badge ────────────────────────────────────────────────────────── */

const GLYPH: Record<Severity, string> = {
  healthy: "✓",
  progressing: "→",
  warning: "!",
  degraded: "▲",
  critical: "✕",
  unknown: "?",
};

const LABEL: Record<Severity, string> = {
  healthy: "Healthy",
  progressing: "Progressing",
  warning: "Warning",
  degraded: "Degraded",
  critical: "Critical",
  unknown: "Unknown",
};

/** 색 단독으로 의미를 전달하지 않습니다. 글리프 + 텍스트가 항상 따라옵니다. */
export function StatusBadge({
  severity,
  label,
  count,
  small,
}: {
  severity: Severity;
  label?: string;
  count?: number;
  small?: boolean;
}) {
  return (
    <span className={`ds-status-badge ds-status-badge--${severity}${small ? " ds-status-badge--sm" : ""}`}>
      <span className="ds-status-badge__glyph" aria-hidden="true">
        {GLYPH[severity]}
      </span>
      <span className="ds-status-badge__label">{label ?? LABEL[severity]}</span>
      {count !== undefined && <span className="ds-status-badge__count">{count}</span>}
    </span>
  );
}

export function StatusDot({ severity }: { severity: Severity }) {
  return <span className="ds-status-dot" style={{ ["--_status-color" as string]: `var(--status-${severity})` }} />;
}

/* ── Stat tile ───────────────────────────────────────────────────────────── */

export function StatTile({
  label,
  value,
  unit,
  tone,
  delta,
  footnote,
  stale,
}: {
  label: string;
  value: ReactNode;
  unit?: string;
  tone?: "warning" | "degraded" | "critical";
  delta?: { text: string; kind: "good" | "bad" | "flat" };
  footnote?: string;
  stale?: boolean;
}) {
  return (
    <div className={`ds-stat-tile${tone ? ` ds-stat-tile--${tone}` : ""}${stale ? " ds-stat-tile--stale" : ""}`}>
      <span className="ds-stat-tile__label">{label}</span>
      <span className="ds-stat-tile__value-row">
        <span className="ds-stat-tile__value">{value}</span>
        {unit && <span className="ds-stat-tile__unit">{unit}</span>}
        {delta && <span className={`ds-stat-tile__delta ds-stat-tile__delta--${delta.kind}`}>{delta.text}</span>}
      </span>
      {footnote && <span className="ds-stat-tile__footnote">{footnote}</span>}
    </div>
  );
}

/* ── Panel ───────────────────────────────────────────────────────────────── */

/**
 * 패널 껍데기. 섹션이 degraded면 테두리와 stale 배지로 알리되 값은 계속 보여줍니다.
 * 한 데이터소스가 죽어도 화면 전체가 무너지지 않아야 합니다. (README §14 완료 기준)
 */
export function Panel({
  title,
  subtitle,
  actions,
  section,
  referenceIso,
  flush,
  children,
}: {
  title: ReactNode;
  subtitle?: ReactNode;
  actions?: ReactNode;
  section?: Section<unknown>;
  referenceIso?: string;
  flush?: boolean;
  children: ReactNode;
}) {
  const degraded = section?.status === "degraded";
  const stale = degraded && section?.observedAt && referenceIso;
  return (
    <section className={`panel${degraded ? " panel--degraded" : ""}`}>
      <header className="panel__header">
        <div>
          <div className="panel__title">{title}</div>
          {subtitle && <div className="panel__subtitle">{subtitle}</div>}
        </div>
        <div className="panel__actions">
          {degraded && (
            <span className="stale-badge" title={section?.reason}>
              <span aria-hidden="true">▲</span>
              {sourceLabel(section?.source) ?? "일부 장애"}
              {stale && ` · ${since(section!.observedAt!, referenceIso!)} 값`}
            </span>
          )}
          {actions}
        </div>
      </header>
      <div className={`panel__body${flush ? " panel__body--flush" : ""}`}>{children}</div>
    </section>
  );
}
