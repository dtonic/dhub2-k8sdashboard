import { Link, useLocation } from "react-router-dom";
import type {
  AlertSummary,
  ClusterEvent,
  EntityRef,
  TopologySummary,
  UnhealthyEntity,
} from "@k8s-dashboard/contracts";
import { StatusBadge, StatusDot } from "@/components/primitives";
import { withSearch } from "@/components/drill";
import { duration, num, since } from "@/lib/format";

/**
 * 이상 엔티티까지 **2회 이내 클릭**으로 도달해야 합니다. (이슈 #14 완료 기준)
 * 그래서 목록의 이름 자체가 상세 화면 링크입니다. 별도의 "자세히" 단계를 두지 않습니다.
 * 현재 URL의 cluster·range·refresh를 유지해야 이동한 화면이 같은 Scope를 봅니다. (#27)
 */
export function detailPath(ref: EntityRef, search: string): string {
  const extra: Record<string, string> = {};
  if (ref.namespace && ref.namespace !== "—") extra.ns = ref.namespace;
  if (ref.podUid) extra.uid = ref.podUid;
  if (ref.podName && ref.namespace && ref.namespace !== "—") {
    return withSearch(`/pods/${encodeURIComponent(ref.podName)}`, search, extra);
  }
  if (ref.workloadName && ref.workloadKind) {
    return withSearch(`/workloads/${ref.workloadKind}/${encodeURIComponent(ref.workloadName)}`, search, extra);
  }
  /* Node는 아직 상세 화면이 없습니다. Namespace로 보내지 않고 목록으로 되돌립니다. */
  if (!ref.namespace || ref.namespace === "—") return withSearch("/namespaces", search);
  return withSearch(`/namespaces/${encodeURIComponent(ref.namespace)}`, search);
}

/* ── Unhealthy Top N ─────────────────────────────────────────────────────── */

const SEVERITY_ORDER = { critical: 0, degraded: 1, warning: 2, progressing: 3, unknown: 4, healthy: 5 } as const;

export function UnhealthyTable({ items, referenceIso }: { items: UnhealthyEntity[]; referenceIso: string }) {
  const { search } = useLocation();
  const sorted = [...items].sort((a, b) => SEVERITY_ORDER[a.severity] - SEVERITY_ORDER[b.severity]);
  return (
    <div className="panel__scroll">
      <table className="ds-data-table ds-data-table--compact">
        <thead>
          <tr>
            <th>이름</th>
            <th style={{ width: 84 }}>종류</th>
            <th style={{ width: 120 }}>Namespace</th>
            <th>상태</th>
            <th className="ds-num" style={{ width: 88 }}>
              Restarts
            </th>
            <th style={{ width: 96 }}>지속</th>
          </tr>
        </thead>
        <tbody>
          {sorted.map((u) => (
            <tr key={`${u.kind}-${u.name}`}>
              <td className="ds-ident">
                <Link to={detailPath(u.ref, search)}>{u.name}</Link>
              </td>
              <td>{u.kind}</td>
              <td>{u.namespace}</td>
              <td>
                <StatusBadge severity={u.severity} label={u.reason} small />
              </td>
              <td className="ds-num">{u.restarts}</td>
              <td className="num">{duration(u.forSeconds)}</td>
            </tr>
          ))}
        </tbody>
      </table>
      <span className="visually-hidden">기준 시각 {referenceIso}</span>
    </div>
  );
}

/* ── Event feed ──────────────────────────────────────────────────────────── */

export function EventFeed({ events, referenceIso }: { events: ClusterEvent[]; referenceIso: string }) {
  const { search } = useLocation();
  return (
    <div className="panel__scroll">
      <table className="ds-data-table ds-data-table--compact">
        <thead>
          <tr>
            <th style={{ width: 92 }}>유형</th>
            <th style={{ width: 150 }}>Reason</th>
            <th>메시지</th>
            <th className="ds-num" style={{ width: 56 }}>
              횟수
            </th>
            <th style={{ width: 88 }}>마지막</th>
          </tr>
        </thead>
        <tbody>
          {events.map((e) => (
            <tr key={e.id}>
              <td>
                <StatusBadge severity={e.type === "Warning" ? "warning" : "healthy"} label={e.type} small />
              </td>
              <td>{e.reason}</td>
              <td className="ds-ident" style={{ maxWidth: 460 }} title={e.message}>
                <Link to={detailPath(e.involved, search)}>{e.involvedName}</Link> <span className="muted">{e.message}</span>
              </td>
              <td className="ds-num">{e.count}</td>
              <td className="num muted">{since(e.lastSeen, referenceIso)}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

/* ── Alert summary ───────────────────────────────────────────────────────── */

export function AlertSummaryCard({ alerts, referenceIso }: { alerts: AlertSummary; referenceIso: string }) {
  const chips = [
    { key: "critical", label: "Critical", severity: "critical" as const, count: alerts.bySeverity.critical },
    { key: "warning", label: "Warning", severity: "warning" as const, count: alerts.bySeverity.warning },
    { key: "info", label: "Info", severity: "progressing" as const, count: alerts.bySeverity.info },
  ];
  return (
    <div style={{ display: "flex", flexDirection: "column", gap: "var(--space-5)" }}>
      <div className="severity-bar">
        {chips.map((c) => (
          <span key={c.key} className="severity-chip">
            <StatusDot severity={c.severity} />
            <span className="severity-chip__count">{c.count}</span>
            <span className="severity-chip__label">{c.label}</span>
          </span>
        ))}
      </div>
      {alerts.top.length > 0 && (
        <table className="ds-data-table ds-data-table--compact">
          <thead>
            <tr>
              <th>알림</th>
              <th style={{ width: 110 }}>Namespace</th>
              <th style={{ width: 88 }}>지속</th>
            </tr>
          </thead>
          <tbody>
            {alerts.top.map((a) => (
              <tr key={a.id}>
                <td>
                  <StatusBadge
                    severity={a.severity === "info" ? "progressing" : a.severity}
                    label={a.name}
                    small
                  />
                </td>
                <td>{a.namespace}</td>
                <td className="num muted">{since(a.activeSince, referenceIso)}</td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </div>
  );
}

/* ── Topology summary ────────────────────────────────────────────────────── */

/**
 * 메인 대시보드에는 **문제 경로만** 축약해 보여주고, 전체 그래프는 Topology 화면으로 넘깁니다.
 * 여기서 그래프를 다 그리면 Overview의 "한 화면에서 판단한다"는 목적이 흐려집니다.
 */
export function TopologySummaryCard({ topology }: { topology: TopologySummary }) {
  const { search } = useLocation();
  return (
    <div style={{ display: "flex", flexDirection: "column", gap: "var(--space-4)" }}>
      <div className="row row--wrap muted" style={{ font: "var(--type-meta)" }}>
        <span>
          Pods <strong className="num">{num(topology.pods)}</strong>
        </span>
        <span aria-hidden="true">·</span>
        <span>
          통신 경로 <strong className="num">{num(topology.edges)}</strong>
        </span>
      </div>
      {topology.problemEdges.length === 0 ? (
        <div className="state" style={{ minHeight: 120 }}>
          <span className="state__glyph" aria-hidden="true">
            ✓
          </span>
          <span className="state__title">문제 있는 통신 경로 없음</span>
        </div>
      ) : (
        <table className="ds-data-table ds-data-table--compact">
          <thead>
            <tr>
              <th>경로</th>
              <th style={{ width: 72 }}>프로토콜</th>
              <th className="ds-num" style={{ width: 76 }}>
                RPS
              </th>
              <th className="ds-num" style={{ width: 84 }}>
                에러율
              </th>
            </tr>
          </thead>
          <tbody>
            {topology.problemEdges.map((e) => (
              <tr key={`${e.from}->${e.to}`}>
                <td className="ds-ident">
                  <StatusDot severity={e.severity} /> {e.from} <span className="muted">→</span> {e.to}
                </td>
                <td>{e.protocol}</td>
                <td className="ds-num">{num(e.requestsPerSecond)}</td>
                <td className="ds-num">{(e.errorRate * 100).toFixed(1)}%</td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
      <Link to={withSearch("/topology", search)}>전체 토폴로지 열기 →</Link>
    </div>
  );
}
