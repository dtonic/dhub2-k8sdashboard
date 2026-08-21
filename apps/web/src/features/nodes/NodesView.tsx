import { useState } from "react";
import { Link, useLocation } from "react-router-dom";
import type { NodeSummary } from "@k8s-dashboard/contracts";
import { drillKeys, useNodeList } from "@/api/queries";
import { Panel, StatusBadge } from "@/components/primitives";
import { SectionView } from "@/components/SectionState";
import { UsageBar, withSearch } from "@/components/drill";
import { duration, num } from "@/lib/format";
import { useDashboardParams } from "@/state/useDashboardParams";
import { PageError, PageHeader, useDrillControls, useInvalidate } from "../drill/common";

/**
 * Nodes — 클러스터의 노드 목록·용량 대비 요청량·노드별 Pod 배치입니다.
 * requested/limits는 스케줄러 관점(종료되지 않은 Pod 전체)이며, 실측 사용량
 * 시계열은 메트릭 데이터소스 연동(후속) 몫입니다. 노드는 클러스터 스코프
 * 리소스라 namespace 제한 사용자에게는 서버가 권한 없음 섹션을 내려보냅니다.
 */
export function NodesView() {
  const { search } = useLocation();
  const { clusterId, refreshMs } = useDashboardParams();
  const q = useNodeList(clusterId, refreshMs);
  const invalidate = useInvalidate(drillKeys.nodes(clusterId));
  const { controls } = useDrillControls(invalidate, q.isFetching, q.dataUpdatedAt || undefined);

  const list = q.data?.nodes.data ?? [];
  const readyCount = list.filter((n) => n.ready).length;

  return (
    <div className="page">
      <PageHeader
        title="Nodes"
        subtitle={list.length ? `${clusterId} · ${readyCount}/${list.length} Ready` : clusterId}
        controls={controls}
      />
      {q.isError ? (
        <PageError error={q.error} onRetry={invalidate} />
      ) : (
        <Panel
          title="노드 상태"
          subtitle="용량 대비 요청량은 스케줄된 Pod의 request 합계입니다"
          section={q.data?.nodes}
          referenceIso={q.data?.generatedAt}
          flush
        >
          <SectionView
            section={q.data?.nodes}
            loading={q.isLoading}
            emptyTitle="노드가 없습니다"
            emptyDetail="이 클러스터에서 조회 가능한 노드가 없습니다."
          >
            {() => <NodeTable items={list} search={search} />}
          </SectionView>
        </Panel>
      )}
    </div>
  );
}

function NodeTable({ items, search }: { items: NodeSummary[]; search: string }) {
  const [open, setOpen] = useState<Record<string, boolean>>({});
  return (
    <div className="panel__scroll" style={{ maxHeight: 680 }}>
      <table className="ds-data-table">
        <thead>
          <tr>
            <th>Node</th>
            <th style={{ width: 110 }}>상태</th>
            <th className="ds-num" style={{ width: 110 }}>
              Pods
            </th>
            <th style={{ width: 210 }}>CPU / 할당가능</th>
            <th style={{ width: 210 }}>Memory / 할당가능</th>
            <th style={{ width: 130 }}>kubelet</th>
            <th className="ds-num" style={{ width: 90 }}>
              나이
            </th>
            <th style={{ width: 96 }} aria-label="Pod 목록 펼치기" />
          </tr>
        </thead>
        <tbody>
          {items.map((n) => {
            const expanded = Boolean(open[n.name]);
            const cpuRatio = n.allocatable.cpuMilli > 0 ? n.requested.cpuMilli / n.allocatable.cpuMilli : null;
            const memRatio = n.allocatable.memoryMib > 0 ? n.requested.memoryMib / n.allocatable.memoryMib : null;
            return [
              <tr key={n.name}>
                <td className="ds-ident">
                  {n.name}
                  <div className="muted" style={{ fontSize: 12 }}>
                    {n.roles.length ? n.roles.join(" · ") : "역할 없음"}
                    {n.internalIP ? ` · ${n.internalIP}` : ""}
                    {n.unschedulable ? " · 스케줄 중지(cordon)" : ""}
                  </div>
                </td>
                <td>
                  <StatusBadge severity={n.severity} small />
                </td>
                <td className="ds-num">
                  {num(n.podsTotal)}/{num(n.allocatable.pods)}
                </td>
                <td>
                  {/* 노드 request/allocatable은 1을 넘지 않으므로 풀스케일로 그립니다. (#14) */}
                  <UsageBar ratio={cpuRatio} label="Request" max={1} />
                  <div className="muted" style={{ fontSize: 12 }}>
                    {num(n.requested.cpuMilli)}m / {num(n.allocatable.cpuMilli)}m
                  </div>
                </td>
                <td>
                  <UsageBar ratio={memRatio} label="Request" max={1} />
                  <div className="muted" style={{ fontSize: 12 }}>
                    {num(n.requested.memoryMib)} Mi / {num(n.allocatable.memoryMib)} Mi
                  </div>
                </td>
                <td className="muted">{n.kubeletVersion}</td>
                <td className="ds-num">{duration(n.ageSeconds)}</td>
                <td>
                  <button
                    type="button"
                    className="ds-button ds-button--ghost"
                    aria-expanded={expanded}
                    onClick={() => setOpen((prev) => ({ ...prev, [n.name]: !expanded }))}
                  >
                    {expanded ? "Pod 접기" : `Pod ${num(n.pods.length)}개`}
                  </button>
                </td>
              </tr>,
              expanded ? (
                <tr key={`${n.name}-pods`}>
                  <td colSpan={8} style={{ padding: 0 }}>
                    <NodePodTable node={n} search={search} />
                  </td>
                </tr>
              ) : null,
            ];
          })}
        </tbody>
      </table>
    </div>
  );
}

function NodePodTable({ node, search }: { node: NodeSummary; search: string }) {
  if (node.pods.length === 0) {
    return (
      <div className="state">
        <span className="state__glyph" aria-hidden="true">
          ✓
        </span>
        <span className="state__title">이 노드에 스케줄된 Pod가 없습니다</span>
      </div>
    );
  }
  return (
    <div className="panel__scroll" style={{ maxHeight: 320 }}>
      <table className="ds-data-table">
        <thead>
          <tr>
            <th>Pod</th>
            <th>Namespace</th>
            <th style={{ width: 110 }}>상태</th>
            <th className="ds-num" style={{ width: 90 }}>
              재시작
            </th>
            <th className="ds-num" style={{ width: 130 }}>
              CPU Request
            </th>
            <th className="ds-num" style={{ width: 140 }}>
              Memory Request
            </th>
          </tr>
        </thead>
        <tbody>
          {node.pods.map((p) => (
            <tr key={p.uid}>
              <td className="ds-ident">
                <Link to={withSearch(`/pods/${encodeURIComponent(p.name)}`, search, { ns: p.namespace, uid: p.uid })}>
                  {p.name}
                </Link>
              </td>
              <td className="muted">{p.namespace}</td>
              <td>
                <StatusBadge severity={p.severity} small />
              </td>
              <td className="ds-num">{num(p.restarts)}</td>
              <td className="ds-num">{num(p.cpuRequestMilli)}m</td>
              <td className="ds-num">{num(p.memoryRequestMib)} Mi</td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}
