import { useEffect } from "react";
import { Link, useLocation, useSearchParams } from "react-router-dom";
import { ISSUE_LABEL, type IssueReason, type NamespaceSummary } from "@k8s-dashboard/contracts";
import { useNamespaceList, drillKeys } from "@/api/queries";
import { Panel, StatusBadge } from "@/components/primitives";
import { SectionView } from "@/components/SectionState";
import { IssueFilter, UsageBar, withSearch } from "@/components/drill";
import { num } from "@/lib/format";
import { useDashboardParams } from "@/state/useDashboardParams";
import { PageError, PageHeader, useDrillControls, useInvalidate, useIssueFilter } from "./common";

/** Namespace 목록 — Cluster Overview 다음 단계. 여기서 문제 Namespace를 고릅니다. */
export function NamespaceList() {
  const { search } = useLocation();
  const { clusterId, range, refreshMs } = useDashboardParams();
  const q = useNamespaceList(clusterId, range, refreshMs);
  const invalidate = useInvalidate(drillKeys.namespaces(clusterId, range));
  /* 이 화면의 주제가 Namespace라 상단 ns 셀렉터는 중복이고, 목록에도 아무 효과가
     없으면서 URL에 ns만 남겨 이후 이동을 오염시킵니다 — 숨깁니다. (#2) */
  const { namespace, patch, controls } = useDrillControls(invalidate, q.isFetching, q.dataUpdatedAt || undefined, {
    showNamespaceSelector: false,
  });

  /* 다른 화면에서 딸려 온 ns 파라미터는 이 화면에서 의미가 없고, 남겨 두면
     상세 링크로 새어 들어가 "셀렉터는 X, 내용은 Y" 불일치를 만듭니다. 정리합니다. (#2) */
  useEffect(() => {
    if (namespace !== "all") patch({ ns: "all" });
  }, [namespace, patch]);

  /* 이름 검색은 사용자 상태이므로 URL(q)에 둡니다 — 자동 갱신이 지우지 않습니다. */
  const [params, setParams] = useSearchParams();
  const query = params.get("q") ?? "";
  const setQuery = (v: string) =>
    setParams(
      (prev) => {
        const p = new URLSearchParams(prev);
        if (v) p.set("q", v);
        else p.delete("q");
        return p;
      },
      { replace: true },
    );

  const filter = useIssueFilter();

  const list = q.data?.namespaces.data ?? [];
  const available = [...new Set(list.flatMap((n) => n.issues))] as IssueReason[];
  const counts = Object.fromEntries(
    available.map((r) => [r, list.filter((n) => n.issues.includes(r)).length]),
  ) as Partial<Record<IssueReason, number>>;

  const needle = query.trim().toLowerCase();
  const shown = list.filter(
    (n) =>
      (!needle || n.name.toLowerCase().includes(needle)) &&
      (!filter.selected.length || filter.selected.every((r) => n.issues.includes(r))),
  );

  return (
    <div className="page">
      <PageHeader
        title="Namespaces"
        subtitle={`${clusterId} · 접근 가능한 Namespace만 표시됩니다`}
        controls={controls}
        actions={
          <>
            <label className="field">
              <span className="visually-hidden">Namespace 이름 검색</span>
              <input
                type="search"
                value={query}
                placeholder="Namespace 이름 검색"
                onChange={(e) => setQuery(e.target.value)}
              />
            </label>
            <IssueFilter
              available={available}
              selected={filter.selected}
              counts={counts}
              onToggle={filter.toggle}
              onClear={filter.clear}
            />
          </>
        }
      />

      {q.isError ? (
        <PageError error={q.error} onRetry={invalidate} />
      ) : (
        <Panel
          title="Namespace 상태"
          subtitle={`${shown.length}/${list.length}개 표시`}
          section={q.data?.namespaces}
          referenceIso={q.data?.generatedAt}
          flush
        >
          <SectionView
            section={q.data?.namespaces}
            loading={q.isLoading}
            emptyTitle="Namespace가 없습니다"
            emptyDetail="이 클러스터에서 접근 가능한 Namespace가 없습니다."
          >
            {() => <NamespaceTable items={shown} search={search} />}
          </SectionView>
        </Panel>
      )}
    </div>
  );
}

function NamespaceTable({ items, search }: { items: NamespaceSummary[]; search: string }) {
  /* 상세 링크에 이 화면 전용 파라미터(ns 잔여물·이름 검색어)를 넘기지 않습니다 —
     ns가 새어 들어가면 상세 화면의 셀렉터와 내용이 어긋납니다. (#2) */
  const cleaned = new URLSearchParams(search);
  cleaned.delete("ns");
  cleaned.delete("q");
  const cleanedSearch = cleaned.toString();

  if (items.length === 0) {
    return (
      <div className="state">
        <span className="state__glyph" aria-hidden="true">
          ✓
        </span>
        <span className="state__title">필터에 맞는 Namespace가 없습니다</span>
        <span className="state__detail">이름 검색어 또는 선택한 상태 조건을 만족하는 Namespace가 없습니다.</span>
      </div>
    );
  }
  return (
    <div className="panel__scroll" style={{ maxHeight: 620 }}>
      <table className="ds-data-table">
        <thead>
          <tr>
            <th>Namespace</th>
            <th style={{ width: 120 }}>상태</th>
            <th className="ds-num" style={{ width: 110 }}>
              Workload
            </th>
            <th className="ds-num" style={{ width: 110 }}>
              Pod
            </th>
            <th className="ds-num" style={{ width: 90 }}>
              재시작
            </th>
            <th style={{ width: 190 }}>CPU / Request</th>
            <th style={{ width: 190 }}>Memory / Request</th>
            <th>문제</th>
          </tr>
        </thead>
        <tbody>
          {items.map((n) => (
            <tr key={n.name}>
              <td className="ds-ident">
                <Link to={withSearch(`/namespaces/${encodeURIComponent(n.name)}`, cleanedSearch)}>{n.name}</Link>
              </td>
              <td>
                <StatusBadge severity={n.severity} small />
              </td>
              <td className="ds-num">
                {num(n.workloads.total - n.workloads.unhealthy)}/{num(n.workloads.total)}
              </td>
              <td className="ds-num">
                {num(n.pods.running)}/{num(n.pods.total)}
              </td>
              <td className="ds-num">{num(n.pods.restarts)}</td>
              <td>
                <UsageBar ratio={n.usage.cpuVsRequest} label="CPU Request" />
              </td>
              <td>
                <UsageBar ratio={n.usage.memoryVsRequest} label="Memory Request" />
              </td>
              <td className="muted">
                {n.issues.length ? n.issues.map((i) => ISSUE_LABEL[i]).join(" · ") : "—"}
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}
