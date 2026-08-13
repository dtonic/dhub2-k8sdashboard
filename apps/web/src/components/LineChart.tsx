import { useId, useMemo, useState } from "react";
import type { TrendSeries } from "@k8s-dashboard/contracts";
import { axisTime, clock, dayClock, num, unitFormat } from "@/lib/format";

/**
 * 시계열 라인 차트.
 * --------------------------------------------------------------------------
 * 차트 라이브러리는 아직 확정되지 않았으므로(ECharts vs uPlot — 별도 ADR),
 * 화면이 라이브러리 API에 직접 묶이지 않도록 얇은 자체 구현을 둡니다.
 * 교체 시 이 파일만 갈아끼우면 됩니다.
 *
 * dataviz 규칙
 * - 이중 y축 없음. 단위가 다른 지표는 패널을 나눕니다.
 * - 계열 색은 고정 슬롯 순서로만 배정하고 순환시키지 않습니다.
 * - 2계열 이상이면 범례가 항상 있고, 마지막 값에 직접 라벨을 붙입니다.
 * - 값·라벨 텍스트는 계열 색을 입지 않습니다. 스와치가 식별을 담당합니다.
 * - 호버 레이어(크로스헤어 + 툴팁)는 기본 제공입니다.
 * - 축 눈금은 숫자만 씁니다. 단위는 패널 부제에 한 번만 적습니다.
 */

const W = 720;
const H = 210;
const X1 = 700;
const Y0 = 14;
const Y1 = 168;

function niceMax(v: number) {
  if (v <= 0) return 1;
  const p = Math.pow(10, Math.floor(Math.log10(v)));
  return (Math.ceil((v / p) * 2) / 2) * p;
}

/** 눈금 라벨: 단위 기호 없이 숫자만. 축이 좁아지고 값 비교가 쉬워집니다. */
function tickLabel(unit: TrendSeries["unit"], v: number) {
  if (unit === "percent") return `${Math.round(v)}%`;
  if (v >= 100 || Number.isInteger(v)) return num(v);
  return v.toFixed(1);
}

export function LineChart({
  series,
  stepSeconds,
  ariaLabel,
}: {
  series: TrendSeries[];
  stepSeconds: number;
  ariaLabel: string;
}) {
  const id = useId();
  const [hover, setHover] = useState<number | null>(null);

  const n = series[0]?.points.length ?? 0;
  const max = useMemo(() => niceMax(Math.max(...series.flatMap((s) => s.points.map((p) => p.v)), 0)), [series]);

  const unit = series[0]?.unit ?? "count";
  const ticks = [0, 0.25, 0.5, 0.75, 1];
  const labels = ticks.map((f) => tickLabel(unit, max * f));
  /* 축 라벨이 잘리지 않도록 가장 긴 라벨 기준으로 왼쪽 여백을 잡습니다. */
  const X0 = 14 + Math.max(...labels.map((l) => l.length)) * 6.4;

  if (n === 0) return null;

  const px = (i: number) => X0 + ((X1 - X0) * i) / Math.max(1, n - 1);
  const py = (v: number) => Y1 - (Y1 - Y0) * (v / max);
  const xTickIdx = [0, 0.25, 0.5, 0.75, 1].map((f) => Math.round(f * (n - 1)));

  /* 마지막 값이 가장 큰 계열에만 직접 라벨을 답니다. */
  const lastValues = series.map((s) => s.points[n - 1]!.v);
  const topIdx = lastValues.indexOf(Math.max(...lastValues));
  const directLabel = `${series[topIdx]!.label} ${unitFormat(unit, lastValues[topIdx]!)}`;
  const directW = directLabel.length * 6.2 + 10;
  const directY = Math.max(py(lastValues[topIdx]!) - 14, Y0 + 11);

  const hoverPoint = hover !== null ? Math.min(n - 1, Math.max(0, hover)) : null;

  return (
    <div style={{ position: "relative" }}>
      {series.length > 1 && (
        <ul className="ds-chart-legend" style={{ marginBottom: "var(--space-4)" }}>
          {series.map((s, i) => (
            <li key={s.key} className={`ds-chart-legend__item ds-series-${i + 1}`}>
              <span className="ds-chart-legend__swatch" />
              {s.label}
            </li>
          ))}
        </ul>
      )}

      <svg
        viewBox={`0 0 ${W} ${H}`}
        style={{ width: "100%", height: "auto", display: "block" }}
        role="img"
        aria-label={ariaLabel}
        onMouseLeave={() => setHover(null)}
        onMouseMove={(ev) => {
          const box = ev.currentTarget.getBoundingClientRect();
          const vx = ((ev.clientX - box.left) / box.width) * W;
          if (vx < X0 || vx > X1) return setHover(null);
          setHover(Math.round(((vx - X0) / (X1 - X0)) * (n - 1)));
        }}
      >
        {ticks.map((f, i) => {
          const y = Y1 - (Y1 - Y0) * f;
          return (
            <g key={f}>
              <line className={f === 0 ? "ds-chart-axis-line" : "ds-chart-grid-line"} x1={X0} y1={y} x2={X1} y2={y} />
              <text className="ds-chart-axis-label" x={X0 - 8} y={y + 4} textAnchor="end">
                {labels[i]}
              </text>
            </g>
          );
        })}

        {xTickIdx.map((i, k) => (
          <text key={`${id}-x${k}`} className="ds-chart-axis-label" x={px(i)} y={Y1 + 22} textAnchor="middle">
            {axisTime(series[0]!.points[i]!.t, stepSeconds)}
          </text>
        ))}

        {series.map((s, i) => (
          <polyline
            key={s.key}
            className="ds-chart-line"
            stroke={`var(--series-${i + 1})`}
            points={s.points.map((p, k) => `${px(k).toFixed(1)},${py(p.v).toFixed(1)}`).join(" ")}
          />
        ))}

        {hoverPoint !== null && (
          <line className="ds-chart-axis-line" x1={px(hoverPoint)} y1={Y0} x2={px(hoverPoint)} y2={Y1} />
        )}

        <circle
          cx={px(n - 1)}
          cy={py(lastValues[topIdx]!)}
          r={4}
          fill={`var(--series-${topIdx + 1})`}
          className="ds-chart-mark--overlap"
        />
        {/* 라벨이 선 위에 겹쳐 읽히지 않는 일을 막기 위해 표면색 받침을 깝니다. */}
        <rect
          x={px(n - 1) - 10 - directW}
          y={directY - 11}
          width={directW}
          height={15}
          rx={4}
          fill="var(--chart-surface)"
        />
        <text
          className="ds-chart-axis-label"
          x={px(n - 1) - 14}
          y={directY}
          textAnchor="end"
          style={{ fill: "var(--color-text-secondary)" }}
        >
          {directLabel}
        </text>
      </svg>

      {hoverPoint !== null && (
        <div
          className="ds-chart-tooltip"
          style={{ left: `min(calc(${(px(hoverPoint) / W) * 100}% + 12px), calc(100% - 230px))`, top: 0 }}
        >
          <div style={{ font: "var(--type-label)", marginBottom: "var(--space-3)" }}>
            {stepSeconds >= 3600 ? dayClock(series[0]!.points[hoverPoint]!.t) : clock(series[0]!.points[hoverPoint]!.t)}
          </div>
          {series.map((s, i) => (
            <div key={s.key} className={`ds-chart-tooltip__row ds-series-${i + 1}`}>
              <span className="ds-chart-tooltip__name">
                <span className="ds-chart-legend__swatch" />
                {s.label}
              </span>
              <span>{unitFormat(s.unit, s.points[hoverPoint]!.v)}</span>
            </div>
          ))}
        </div>
      )}
    </div>
  );
}
