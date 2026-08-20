import { RANGE_LABEL, STEP_LABEL, type RangeKey, type ScopeResponse } from "@k8s-dashboard/contracts";
import { REFRESH_OPTIONS } from "@/state/useDashboardParams";
import { clock } from "@/lib/format";
import { Combobox } from "./Combobox";

/* ── Scope Selector ──────────────────────────────────────────────────────── */

/**
 * 사용자가 볼 수 있는 범위만 노출합니다. 접근 불가한 클러스터는 목록에 남기되
 * 선택할 수 없게 하고 이유를 표시합니다 — 목록에서 통째로 지우면
 * "왜 내 클러스터가 안 보이지?"라는 질문이 반복됩니다.
 *
 * 옵션은 검색형 콤보박스로 노출합니다 — namespace가 수백 개여도 스크롤·검색으로
 * 찾을 수 있습니다. 전체(all) scope의 이름 목록은 서버가 /scope 응답의
 * availableNamespaces로 열거합니다. (#1)
 *
 * 여기서 고른 값은 힌트일 뿐이고, 실제 Scope는 서버가 토큰에서 강제합니다. (README §10)
 */
export function ScopeSelector({
  scope,
  clusterId,
  namespace,
  onChange,
  disabled,
  showNamespace = true,
}: {
  scope: ScopeResponse | undefined;
  clusterId: string;
  namespace: string;
  onChange: (next: { cluster?: string; ns?: string }) => void;
  disabled?: boolean;
  /** Namespace 자체가 화면의 주제인 페이지(/namespaces)는 ns 셀렉터를 숨깁니다. (#2) */
  showNamespace?: boolean;
}) {
  const clusters = scope?.clusters ?? [];
  const current = clusters.find((c) => c.id === clusterId);
  const nsOptions =
    current?.namespaces === "all" ? (current.availableNamespaces ?? []) : (current?.namespaces ?? []);

  return (
    <div className="scope">
      <Combobox
        id="scope-cluster"
        label="클러스터"
        value={clusterId}
        disabled={disabled || clusters.length === 0}
        options={clusters.map((c) => ({
          value: c.id,
          label: c.name,
          disabled: !c.accessible,
          note: c.accessible ? undefined : "권한 없음",
        }))}
        onSelect={(v) => onChange({ cluster: v })}
      />
      {showNamespace && (
        <>
          <span className="scope__sep" aria-hidden="true">
            /
          </span>
          <Combobox
            id="scope-ns"
            label="네임스페이스"
            value={namespace}
            disabled={disabled}
            options={[
              { value: "all", label: "모든 Namespace" },
              ...nsOptions.map((ns) => ({ value: ns, label: ns })),
            ]}
            onSelect={(v) => onChange({ ns: v })}
          />
        </>
      )}
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
