import { RANGE_LABEL, STEP_LABEL, type RangeKey, type ScopeResponse } from "@k8s-dashboard/contracts";
import { REFRESH_OPTIONS } from "@/state/useDashboardParams";
import { clock } from "@/lib/format";

/* ── Scope Selector ──────────────────────────────────────────────────────── */

/**
 * 사용자가 볼 수 있는 범위만 노출합니다. 접근 불가한 클러스터는 목록에 남기되
 * 선택할 수 없게 하고 이유를 표시합니다 — 목록에서 통째로 지우면
 * "왜 내 클러스터가 안 보이지?"라는 질문이 반복됩니다.
 *
 * 여기서 고른 값은 힌트일 뿐이고, 실제 Scope는 서버가 토큰에서 강제합니다. (README §10)
 */
export function ScopeSelector({
  scope,
  clusterId,
  namespace,
  namespaces,
  onChange,
  disabled,
}: {
  scope: ScopeResponse | undefined;
  clusterId: string;
  namespace: string;
  namespaces: string[];
  onChange: (next: { cluster?: string; ns?: string }) => void;
  disabled?: boolean;
}) {
  const clusters = scope?.clusters ?? [];
  const current = clusters.find((c) => c.id === clusterId);
  const nsOptions = current?.namespaces === "all" ? namespaces : (current?.namespaces ?? []);

  return (
    <div className="scope">
      <label className="visually-hidden" htmlFor="scope-cluster">
        클러스터
      </label>
      <select
        id="scope-cluster"
        value={clusterId}
        disabled={disabled || clusters.length === 0}
        onChange={(e) => onChange({ cluster: e.target.value })}
      >
        {clusters.map((c) => (
          <option key={c.id} value={c.id} disabled={!c.accessible}>
            {c.name}
            {c.accessible ? "" : " (권한 없음)"}
          </option>
        ))}
      </select>
      <span className="scope__sep" aria-hidden="true">
        /
      </span>
      <label className="visually-hidden" htmlFor="scope-ns">
        네임스페이스
      </label>
      <select id="scope-ns" value={namespace} disabled={disabled} onChange={(e) => onChange({ ns: e.target.value })}>
        <option value="all">모든 Namespace</option>
        {nsOptions.map((ns) => (
          <option key={ns} value={ns}>
            {ns}
          </option>
        ))}
      </select>
    </div>
  );
}

/* ── Time Range ──────────────────────────────────────────────────────────── */

const PRESETS: RangeKey[] = ["30d", "7d", "1d", "1h"];

export function TimeRangePicker({ range, onChange }: { range: RangeKey; onChange: (r: RangeKey) => void }) {
  const stepLabel = range === "custom" ? "15분" : STEP_LABEL[range];
  return (
    <div className="ds-time-range">
      <div className="ds-time-range__presets" role="group" aria-label="시간 범위">
        {PRESETS.map((r) => (
          <button
            key={r}
            type="button"
            className="ds-time-range__preset"
            aria-pressed={range === r}
            onClick={() => onChange(r)}
          >
            {RANGE_LABEL[r].replace("최근 ", "")}
          </button>
        ))}
        <button
          type="button"
          className="ds-time-range__preset"
          aria-pressed={range === "custom"}
          onClick={() => onChange("custom")}
        >
          Custom
        </button>
      </div>
      <span className="ds-time-range__state">
        <span className="ds-time-range__step">step {stepLabel}</span>
      </span>
    </div>
  );
}

/* ── Refresh ─────────────────────────────────────────────────────────────── */

/**
 * 자동 갱신은 **데이터만** 갱신합니다. 페이지를 다시 그리지 않습니다.
 * TanStack Query의 `placeholderData: keepPreviousData` 덕분에 값 교체 중에도
 * 레이아웃과 스크롤 위치가 유지됩니다. (이슈 #14 완료 기준)
 */
export function RefreshControl({
  refreshMs,
  onChange,
  onRefreshNow,
  fetching,
  updatedAtMs,
}: {
  refreshMs: number;
  onChange: (ms: number) => void;
  onRefreshNow: () => void;
  fetching: boolean;
  /** 마지막으로 응답을 받은 실제 시각. mock의 관측 시각과 구분합니다. */
  updatedAtMs?: number;
}) {
  return (
    <div className="refresh">
      <label className="visually-hidden" htmlFor="refresh-interval">
        자동 갱신 주기
      </label>
      <select id="refresh-interval" value={refreshMs} onChange={(e) => onChange(Number(e.target.value))}>
        {REFRESH_OPTIONS.map((o) => (
          <option key={o.value} value={o.value}>
            자동 갱신: {o.label}
          </option>
        ))}
      </select>
      <button type="button" className="linkish" onClick={onRefreshNow} aria-live="off">
        지금 갱신
      </button>
      <span aria-live="polite">{fetching ? "갱신 중…" : updatedAtMs ? `마지막 갱신 ${clock(updatedAtMs)}` : ""}</span>
    </div>
  );
}
