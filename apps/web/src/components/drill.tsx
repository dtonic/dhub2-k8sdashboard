import { Link } from "react-router-dom";
import { ISSUE_LABEL, type EntityRef, type IssueReason, type OwnerRef, type ResourceUsage } from "@k8s-dashboard/contracts";
import { num } from "@/lib/format";

/* ── Breadcrumb ──────────────────────────────────────────────────────────── */

/** 드릴다운 경로를 항상 노출합니다. 어디까지 내려왔는지 모르면 되돌아가지 못합니다. */
export function Breadcrumb({ items }: { items: Array<{ label: string; to?: string }> }) {
  return (
    <nav className="crumbs" aria-label="현재 위치">
      {items.map((it, i) => (
        <span key={`${it.label}-${i}`} className="crumbs__item">
          {it.to ? <Link to={it.to}>{it.label}</Link> : <span aria-current="page">{it.label}</span>}
          {i < items.length - 1 && (
            <span className="crumbs__sep" aria-hidden="true">
              /
            </span>
          )}
        </span>
      ))}
    </nav>
  );
}

/* ── 링크 경로 ───────────────────────────────────────────────────────────── */

/** Scope·시간 범위를 유지한 채 이동합니다. (이슈 #15 작업 범위: URL에 상태 유지) */
export function withSearch(path: string, search: string, extra: Record<string, string> = {}) {
  const p = new URLSearchParams(search);
  for (const [k, v] of Object.entries(extra)) p.set(k, v);
  return `${path}?${p}`;
}

export function refPath(ref: EntityRef, search: string): string {
  if (ref.podName) {
    return withSearch(`/pods/${encodeURIComponent(ref.podName)}`, search, {
      ns: ref.namespace ?? "",
      ...(ref.podUid ? { uid: ref.podUid } : {}),
    });
  }
  if (ref.workloadName && ref.workloadKind) {
    return withSearch(`/workloads/${ref.workloadKind}/${encodeURIComponent(ref.workloadName)}`, search, {
      ns: ref.namespace ?? "",
    });
  }
  return withSearch(`/namespaces/${encodeURIComponent(ref.namespace ?? "default")}`, search);
}

/** Logs Explorer로 Pod와 **같은 시간 범위**를 넘깁니다. */
export function logsPath(ref: EntityRef, search: string) {
  return withSearch("/logs", search, {
    ns: ref.namespace ?? "",
    ...(ref.podName ? { pod: ref.podName } : {}),
    ...(ref.podUid ? { uid: ref.podUid } : {}),
  });
}

/* ── 필터 칩 ─────────────────────────────────────────────────────────────── */

/**
 * 상태 필터. 선택은 URL에 남고, 데이터가 갱신되어도 유지됩니다.
 * (이슈 #15 완료 기준: 갱신 시 사용자 선택이 초기화되지 않을 것)
 */
export function IssueFilter({
  available,
  selected,
  counts,
  onToggle,
  onClear,
}: {
  available: IssueReason[];
  selected: IssueReason[];
  counts: Partial<Record<IssueReason, number>>;
  onToggle: (r: IssueReason) => void;
  onClear: () => void;
}) {
  if (available.length === 0) return null;
  return (
    <div className="chips" role="group" aria-label="상태 필터">
      {available.map((r) => (
        <button
          key={r}
          type="button"
          className="chip"
          aria-pressed={selected.includes(r)}
          onClick={() => onToggle(r)}
        >
          {ISSUE_LABEL[r]}
          {counts[r] !== undefined && <span className="chip__count num">{counts[r]}</span>}
        </button>
      ))}
      {selected.length > 0 && (
        <button type="button" className="linkish" onClick={onClear}>
          필터 해제
        </button>
      )}
    </div>
  );
}

/* ── 사용량 막대 ─────────────────────────────────────────────────────────── */

/**
 * Request/Limit 대비 비율. 100%를 기준선으로 두고 넘으면 상태 색으로 칠합니다.
 * 색만으로 판단하지 않도록 수치를 항상 함께 씁니다.
 */
export function UsageBar({
  ratio,
  label,
  max = 2,
}: {
  ratio: number | null;
  label: string;
  /**
   * 막대가 표현하는 최대 비율. 초과 할당이 가능한 화면(사용량 vs request)은 2가
   * 기본이며 중앙 눈금이 100%입니다. 노드 request/allocatable처럼 1을 넘을 수
   * 없는 비율은 1을 지정해 0–100% 풀스케일로 그립니다. (#14)
   */
  max?: 1 | 2;
}) {
  if (ratio === null) {
    return (
      <span className="usage">
        <span className="usage__text muted">{label} 미설정</span>
      </span>
    );
  }
  const pct = (Math.min(ratio, max) / max) * 100;
  const tone = ratio >= 1 ? "critical" : ratio >= 0.85 ? "warning" : "healthy";
  return (
    <span className="usage" title={`${label} 대비 ${(ratio * 100).toFixed(0)}%`}>
      <span className={`usage__track usage__track--${tone}`}>
        <span className="usage__fill" style={{ width: `${pct}%` }} />
        {max > 1 && <span className="usage__mark" />}
      </span>
      <span className="usage__text num">{(ratio * 100).toFixed(0)}%</span>
    </span>
  );
}

export function UsageCell({ usage }: { usage: ResourceUsage }) {
  return (
    <span className="row" style={{ gap: "var(--space-4)" }}>
      <span className="num muted" style={{ minWidth: 78 }}>
        {num(usage.cpuMilli)}m
      </span>
      <UsageBar ratio={usage.cpuVsRequest} label="CPU Request" />
    </span>
  );
}

/* ── OwnerReference 체인 ─────────────────────────────────────────────────── */

/**
 * Deployment → ReplicaSet → Pod 관계를 그대로 보여줍니다.
 * 롤아웃 중에는 ReplicaSet이 둘 이상 공존하므로 현재 세대를 표시합니다.
 */
export function OwnerChain({ chain }: { chain: OwnerRef[] }) {
  return (
    <ul className="owner-chain">
      {chain.map((o) => (
        <li key={o.uid} className={o.current ? "owner-chain__item owner-chain__item--current" : "owner-chain__item"}>
          <span className="owner-chain__kind">{o.kind}</span>
          <span className="ds-ident">{o.name}</span>
          {o.revision && <span className="muted">rev {o.revision}</span>}
          {o.pods !== undefined && <span className="muted num">Pod {o.pods}</span>}
          {o.current ? <span className="owner-chain__badge">현재 세대</span> : <span className="muted">이전 세대</span>}
          <span className="ds-ident muted owner-chain__uid" title={o.uid}>
            {o.uid}
          </span>
        </li>
      ))}
    </ul>
  );
}
