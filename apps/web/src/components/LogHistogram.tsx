import { useRef, useState } from "react";
import { LEVEL_ORDER, type ClusterEvent, type LogLevel } from "@k8s-dashboard/contracts";
import { clock, dayClock, num } from "@/lib/format";

/**
 * 로그 히스토그램 + Event 타임라인 + 구간 선택(brush).
 * --------------------------------------------------------------------------
 * 이슈 #16의 핵심 동선입니다. 그래프의 이상 구간을 드래그하면 그 시간 범위로
 * 로그 조회가 좁혀지고, 같은 축 위에 Kubernetes Event가 함께 찍힙니다.
 *
 * dataviz 규칙
 * - 레벨은 계열이 아니라 **상태**입니다. series 색이 아니라 status 토큰을 씁니다.
 *   ERROR가 항상 같은 빨강이어야 화면 간 학습이 유지됩니다.
 * - 누적 막대는 인접 세그먼트 사이에 2px 표면 간격을 둡니다.
 * - 색 단독으로 구분하지 않도록 범례에 레벨명과 건수를 함께 씁니다.
 */

const W = 1200;
const H = 132;
const X0 = 8;
const X1 = W - 8;
const Y0 = 10;
const Y1 = 96;
const EVENT_Y = 116;

const LEVEL_COLOR: Record<LogLevel, string> = {
  ERROR: "var(--status-critical)",
  WARN: "var(--status-warning)",
  INFO: "var(--color-border-strong)",
  DEBUG: "var(--color-border-default)",
};

export function LogHistogram({
  buckets,
  events,
  from,
  to,
  selection,
  onSelect,
  onClear,
}: {
  buckets: Array<{ t: number; counts: Record<LogLevel, number> }>;
  events: ClusterEvent[];
  from: number;
  to: number;
  selection: { from: number; to: number } | null;
  onSelect: (window: { from: number; to: number }) => void;
  onClear: () => void;
}) {
  const svgRef = useRef<SVGSVGElement>(null);
  const [drag, setDrag] = useState<{ a: number; b: number } | null>(null);
  const [hover, setHover] = useState<number | null>(null);

  const span = Math.max(1, to - from);
  const xOf = (t: number) => X0 + ((X1 - X0) * (t - from)) / span;
  const tOf = (x: number) => from + ((x - X0) / (X1 - X0)) * span;
  const total = (b: (typeof buckets)[number]) => LEVEL_ORDER.reduce((s, l) => s + b.counts[l], 0);
  const max = Math.max(1, ...buckets.map(total));
  const bw = buckets.length > 1 ? (X1 - X0) / buckets.length : X1 - X0;

  const svgX = (ev: React.MouseEvent) => {
    const box = svgRef.current!.getBoundingClientRect();
    return Math.min(X1, Math.max(X0, ((ev.clientX - box.left) / box.width) * W));
  };

  const totals = LEVEL_ORDER.map((l) => ({ level: l, count: buckets.reduce((s, b) => s + b.counts[l], 0) }));
  const hovered = hover !== null ? buckets[hover] : null;

  return (
    <div style={{ position: "relative" }}>
      <div className="row row--wrap" style={{ marginBottom: "var(--space-4)" }}>
        <ul className="ds-chart-legend">
          {totals.map((t) => (
            <li key={t.level} className="ds-chart-legend__item">
              <span className="ds-chart-legend__swatch" style={{ background: LEVEL_COLOR[t.level] }} />
              {t.level} <span className="num muted">{num(t.count)}</span>
            </li>
          ))}
        </ul>
        <span className="spacer" />
        {selection ? (
          <span className="row">
            <span className="stale-badge" style={{ borderColor: "var(--color-border-focus)", color: "var(--color-text-primary)" }}>
              선택 구간 {clock(selection.from)} – {clock(selection.to)}
            </span>
            <button type="button" className="linkish" onClick={onClear}>
              구간 해제
            </button>
          </span>
        ) : (
          <span className="muted" style={{ font: "var(--type-meta)" }}>
            차트를 드래그하면 그 구간으로 로그를 좁힙니다
          </span>
        )}
      </div>

      <svg
        ref={svgRef}
        viewBox={`0 0 ${W} ${H}`}
        style={{ width: "100%", height: "auto", display: "block", cursor: "crosshair" }}
        role="img"
        aria-label={`로그 레벨별 분포와 Kubernetes Event 타임라인. 총 ${num(totals.reduce((s, t) => s + t.count, 0))}건.`}
        onMouseDown={(ev) => setDrag({ a: svgX(ev), b: svgX(ev) })}
        onMouseMove={(ev) => {
          const x = svgX(ev);
          setHover(Math.min(buckets.length - 1, Math.max(0, Math.floor((x - X0) / bw))));
          if (drag) setDrag({ ...drag, b: x });
        }}
        onMouseUp={() => {
          if (!drag) return;
          const [a, b] = [Math.min(drag.a, drag.b), Math.max(drag.a, drag.b)];
          setDrag(null);
          /* 클릭에 가까운 드래그는 선택으로 치지 않습니다. 오조작 방지. */
          if (b - a < 6) return;
          onSelect({ from: Math.round(tOf(a)), to: Math.round(tOf(b)) });
        }}
        onMouseLeave={() => {
          setDrag(null);
          setHover(null);
        }}
      >
        <line className="ds-chart-axis-line" x1={X0} y1={Y1} x2={X1} y2={Y1} />

        {buckets.map((b, i) => {
          let y = Y1;
          return (
            <g key={b.t}>
              {LEVEL_ORDER.map((l) => {
                const c = b.counts[l];
                if (!c) return null;
                const h = ((Y1 - Y0) * c) / max;
                y -= h;
                return (
                  <rect
                    key={l}
                    x={X0 + i * bw + 0.5}
                    y={y}
                    width={Math.max(1, bw - 2)}
                    /* 인접 세그먼트 사이 2px 표면 간격 */
                    height={Math.max(1, h - 2)}
                    fill={LEVEL_COLOR[l]}
                    rx={1}
                  />
                );
              })}
            </g>
          );
        })}

        {/* 같은 축 위의 Kubernetes Event */}
        {events
          .filter((e) => Date.parse(e.lastSeen) >= from && Date.parse(e.lastSeen) <= to)
          .map((e) => (
            <g key={e.id}>
              <line
                x1={xOf(Date.parse(e.lastSeen))}
                y1={Y0}
                x2={xOf(Date.parse(e.lastSeen))}
                y2={Y1}
                stroke={e.type === "Warning" ? "var(--status-degraded)" : "var(--color-border-strong)"}
                strokeWidth={1}
                strokeDasharray="3 3"
              />
              <circle
                cx={xOf(Date.parse(e.lastSeen))}
                cy={EVENT_Y}
                r={5}
                fill={e.type === "Warning" ? "var(--status-degraded)" : "var(--color-border-strong)"}
              >
                <title>{`${e.reason} · ${e.involvedName} · ${dayClock(Date.parse(e.lastSeen))}`}</title>
              </circle>
            </g>
          ))}
        <text className="ds-chart-axis-label" x={X0} y={EVENT_Y + 4}>
          {" "}
        </text>

        {/* 선택 구간이 이미 조회 범위와 같으면 밴드를 덧그리지 않습니다. 화면 전체가 칠해져 의미가 없습니다. */}
        {(drag || (selection && (selection.from > from || selection.to < to))) &&
          (() => {
            const a = drag ? Math.min(drag.a, drag.b) : xOf(selection!.from);
            const b = drag ? Math.max(drag.a, drag.b) : xOf(selection!.to);
            return (
              <rect
                x={a}
                y={Y0}
                width={Math.max(1, b - a)}
                height={Y1 - Y0}
                fill="var(--color-surface-selected)"
                stroke="var(--color-border-focus)"
                strokeWidth={1}
              />
            );
          })()}

        <text className="ds-chart-axis-label" x={X0} y={Y1 + 16}>
          {dayClock(from)}
        </text>
        <text className="ds-chart-axis-label" x={X1} y={Y1 + 16} textAnchor="end">
          {dayClock(to)}
        </text>
      </svg>

      {hovered && (
        <div className="ds-chart-tooltip" style={{ left: `min(${(xOf(hovered.t) / W) * 100}%, calc(100% - 220px))`, top: 0 }}>
          <div style={{ font: "var(--type-label)", marginBottom: "var(--space-3)" }}>{dayClock(hovered.t)}</div>
          {LEVEL_ORDER.filter((l) => hovered.counts[l] > 0).map((l) => (
            <div key={l} className="ds-chart-tooltip__row">
              <span className="ds-chart-tooltip__name">
                <span className="ds-chart-legend__swatch" style={{ background: LEVEL_COLOR[l] }} />
                {l}
              </span>
              <span>{num(hovered.counts[l])}</span>
            </div>
          ))}
        </div>
      )}
    </div>
  );
}
