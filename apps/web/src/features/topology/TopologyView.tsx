import { useRef, useState } from "react";
import { Link, useLocation, useNavigate, useSearchParams } from "react-router-dom";
import { RANGE_LABEL, type TopologyNodePosition } from "@k8s-dashboard/contracts";
import { topoKeys, useEdgeSeries, useSaveTopologyLayout, useTopology } from "@/api/queries";
import { useDashboardParams } from "@/state/useDashboardParams";
import { LineChart } from "@/components/LineChart";
import { Panel, StatTile, StatusBadge } from "@/components/primitives";
import { SectionView } from "@/components/SectionState";
import { Breadcrumb, logsPath, refPath, withSearch } from "@/components/drill";
import { duration, num } from "@/lib/format";
import { PageError, PageHeader, useDrillControls, useInvalidate } from "@/features/drill/common";
import { TopologyGraph } from "./TopologyGraph";

/**
 * Pod Topology 화면
 * --------------------------------------------------------------------------
 * design-system의 topology preview를 앱에 연결한 화면입니다.
 * preview와 달라진 점 하나 — 비정상 Pod를 클릭하면 로그 모달이 아니라
 * **Pod 상세 화면**으로 갑니다. 이제 진짜 상세 화면과 Logs Explorer가 있으므로
 * 축약된 모달을 따로 유지할 이유가 없습니다.
 */
export function TopologyView() {
  const { search } = useLocation();
  const navigate = useNavigate();
  const [params, setParams] = useSearchParams();
  const { clusterId, namespace, range, refreshMs } = useDashboardParams();
  const q = useTopology(clusterId, namespace, range, refreshMs);
  const invalidate = useInvalidate(topoKeys.graph(clusterId, namespace, range));
  const { controls } = useDrillControls(invalidate, q.isFetching, q.dataUpdatedAt || undefined);

  const graph = q.data?.graph.data;
  const selectedEdgeId = params.get("edge") ?? graph?.edges[0]?.id ?? null;
  const edge = graph?.edges.find((e) => e.id === selectedEdgeId) ?? null;
  const [mode, setMode] = useState<"table" | "chart">("table");
  const series = useEdgeSeries(clusterId, mode === "chart" ? selectedEdgeId : null, range);

  /* 공유 배치 편집 (#28) — Edit 모드는 조회 상태가 아니라 도구 상태이므로 URL이 아닌
     로컬 state에 둡니다. 드래그 좌표는 렌더와 무관하므로 ref로 받습니다. */
  const [editMode, setEditMode] = useState(false);
  const [editSession, setEditSession] = useState(0);
  const pendingPositions = useRef<TopologyNodePosition[] | null>(null);
  const saveLayout = useSaveTopologyLayout(clusterId);
  const canEdit = q.data?.canEditLayout ?? false;

  const exitEdit = () => {
    setEditMode(false);
    setEditSession((v) => v + 1); // 편집 중 좌표를 버리고 저장/기본 배치로 되돌립니다.
    pendingPositions.current = null;
  };
  const save = () => {
    const positions = pendingPositions.current;
    if (!positions) return setEditMode(false);
    saveLayout.mutate(positions, {
      onSuccess: () => {
        setEditMode(false);
        pendingPositions.current = null;
      },
    });
  };
  const resetLayout = () => {
    saveLayout.mutate([], { onSuccess: () => exitEdit() });
  };

  const nodeName = (id?: string) => graph?.nodes.find((n) => n.id === id)?.name ?? id ?? "";
  const reverse = edge && graph?.edges.some((e) => e.from === edge.to && e.to === edge.from);
  const pods = q.data?.pods.data;

  return (
    <div className="page">
      <PageHeader
        crumbs={
          <Breadcrumb
            items={[
              { label: "Cluster Overview", to: withSearch("/", search) },
              { label: "Namespaces", to: withSearch("/namespaces", search) },
              { label: "Pod Topology" },
            ]}
          />
        }
        title="Pod Topology"
        subtitle={`${clusterId} · ${namespace === "all" ? "모든 Namespace" : namespace} · ${RANGE_LABEL[range]}`}
        controls={controls}
      />

      {q.isError ? (
        <PageError error={q.error} onRetry={invalidate} />
      ) : (
        <>
          {pods && (
            <div className="grid grid--kpi">
              <StatTile label="Pods" value={num(pods.total)} footnote="현재 그래프에 표시된 Pod" />
              <StatTile label="정상" value={num(pods.healthy)} footnote={`전체 ${num(pods.total)}`} />
              <StatTile
                label="비정상"
                value={num(pods.unhealthy)}
                tone={pods.unhealthy > 0 ? "critical" : undefined}
                footnote={pods.unhealthy > 0 ? "아래 목록에서 이름을 클릭하면 상세로 이동합니다" : "이상 없음"}
              />
            </div>
          )}

          <Panel
            title="통신 그래프"
            subtitle={
              editMode
                ? "편집 모드 — 노드를 드래그해 배치를 바꾼 뒤 저장하세요. 저장된 배치는 모든 사용자에게 적용됩니다."
                : "선을 클릭하면 방향별 상세가 열립니다 · A→B와 B→A는 방향별 별도 선입니다"
            }
            section={q.data?.graph}
            referenceIso={q.data?.generatedAt}
            actions={
              canEdit && (
                <span className="topo-toolbar">
                  {editMode ? (
                    <>
                      {saveLayout.isError && <span className="topo-toolbar__hint" role="alert">저장 실패 — 다시 시도하세요</span>}
                      <button type="button" className="linkish" onClick={resetLayout} disabled={saveLayout.isPending}>
                        기본 배치로
                      </button>
                      <button type="button" className="linkish" onClick={exitEdit} disabled={saveLayout.isPending}>
                        취소
                      </button>
                      <button type="button" onClick={save} disabled={saveLayout.isPending}>
                        {saveLayout.isPending ? "저장 중…" : "배치 저장"}
                      </button>
                    </>
                  ) : (
                    <button type="button" onClick={() => setEditMode(true)}>
                      배치 편집
                    </button>
                  )}
                </span>
              )
            }
          >
            <SectionView
              section={q.data?.graph}
              loading={q.isLoading}
              emptyTitle="통신 데이터가 없습니다"
              emptyDetail="선택한 범위에 관측된 Pod 간 통신이 없습니다."
            >
              {(g) => (
                <>
                  <TopologyGraph
                    nodes={g.nodes}
                    edges={g.edges}
                    savedPositions={q.data?.layout?.positions ?? null}
                    selectedEdgeId={selectedEdgeId}
                    editMode={editMode}
                    editSession={editSession}
                    onPositionsChange={(positions) => {
                      pendingPositions.current = positions;
                    }}
                    onSelectEdge={(id) =>
                      setParams(
                        (prev) => {
                          const p = new URLSearchParams(prev);
                          p.set("edge", id);
                          return p;
                        },
                        { replace: true },
                      )
                    }
                    onSelectNode={(n) => navigate(refPath(n.ref, search))}
                  />
                  <ul className="ds-topology__legend" style={{ marginTop: "var(--space-4)" }}>
                    <li>
                      <i style={{ ["--_c" as string]: "var(--status-healthy)" }} /> 정상
                    </li>
                    <li>
                      <i style={{ ["--_c" as string]: "var(--status-warning)" }} /> 지연/에러 증가
                    </li>
                    <li>
                      <i style={{ ["--_c" as string]: "var(--status-critical)" }} /> 심각
                    </li>
                    <li>선 두께 = 트래픽 양 · 색 = 상태(프로토콜 아님) · 캡슐 텍스트 = 프로토콜과 누적 요청</li>
                  </ul>
                </>
              )}
            </SectionView>
          </Panel>

          <div className="grid grid--split">
            <Panel
              title={
                edge ? (
                  <span className="row">
                    <span className="ds-ident">{nodeName(edge.from)}</span>
                    <span className="muted" aria-hidden="true">
                      →
                    </span>
                    <span className="ds-ident">{nodeName(edge.to)}</span>
                    <StatusBadge severity={edge.severity} small />
                  </span>
                ) : (
                  "방향 상세"
                )
              }
              subtitle={
                edge
                  ? `${RANGE_LABEL[range]} · 누적 ${num(edge.totalCount)}건 · 에러율 ${(edge.errorRate * 100).toFixed(1)}% · ${
                      reverse ? `반대 방향(${nodeName(edge.to)} → ${nodeName(edge.from)})은 별도 선입니다` : "이 방향으로만 관측됩니다"
                    }`
                  : undefined
              }
              actions={
                edge && (
                  <button
                    type="button"
                    className="ds-icon-button"
                    aria-pressed={mode === "chart"}
                    aria-label={mode === "chart" ? "표로 보기" : "시계열 차트로 보기"}
                    title={mode === "chart" ? "표로 보기" : "시계열 차트로 보기"}
                    onClick={() => setMode((m) => (m === "chart" ? "table" : "chart"))}
                  >
                    <svg viewBox="0 0 16 16" aria-hidden="true" fill="none" stroke="currentColor" strokeWidth={1.6} strokeLinecap="round" strokeLinejoin="round">
                      <path d="M2 13.5V2.5" />
                      <path d="M2 13.5h12" />
                      <path d="M4 10.5l3-3 2.5 2.5L14 4.5" />
                    </svg>
                  </button>
                )
              }
              flush={mode === "table"}
            >
              {!edge ? (
                <div className="state">
                  <span className="state__glyph" aria-hidden="true">
                    →
                  </span>
                  <span className="state__title">선을 선택하세요</span>
                </div>
              ) : mode === "table" ? (
                <div className="panel__scroll">
                  <table className="ds-data-table ds-data-table--compact">
                    <thead>
                      <tr>
                        <th style={{ width: 110 }}>통신 종류</th>
                        <th>Route 종류 (혹은 API)</th>
                        <th className="ds-num" style={{ width: 140 }}>
                          누적 Count
                        </th>
                        <th className="ds-num" style={{ width: 100 }}>
                          에러
                        </th>
                      </tr>
                    </thead>
                    <tbody>
                      {edge.routes.map((r) => (
                        <tr key={`${r.protocol}-${r.route}`}>
                          <td>{r.protocol}</td>
                          <td className="ds-ident">{r.route}</td>
                          <td className="ds-num">{num(r.count)}</td>
                          <td className="ds-num">{num(r.errorCount)}</td>
                        </tr>
                      ))}
                      <tr>
                        <td colSpan={2} style={{ font: "var(--type-label)", color: "var(--color-text-secondary)" }}>
                          합계
                        </td>
                        <td className="ds-num" style={{ font: "var(--type-body-strong)" }}>
                          {num(edge.totalCount)}
                        </td>
                        <td className="ds-num">{num(edge.routes.reduce((s, r) => s + r.errorCount, 0))}</td>
                      </tr>
                    </tbody>
                  </table>
                </div>
              ) : (
                <SectionView section={series.data?.series} loading={series.isLoading} emptyTitle="시계열이 없습니다">
                  {(s) => (
                    <>
                      <LineChart
                        series={s}
                        stepSeconds={series.data!.range.stepSeconds}
                        ariaLabel={`${nodeName(edge.from)}에서 ${nodeName(edge.to)}로의 Route별 요청 수 시계열`}
                      />
                      <div className="muted" style={{ font: "var(--type-meta)", marginTop: "var(--space-3)" }}>
                        {RANGE_LABEL[range]} · step{" "}
                        {series.data!.range.stepSeconds >= 3600
                          ? `${series.data!.range.stepSeconds / 3600}시간`
                          : `${series.data!.range.stepSeconds / 60}분`}{" "}
                        · 상위 3개 Route + 나머지는 “기타”로 접음
                      </div>
                    </>
                  )}
                </SectionView>
              )}
            </Panel>

            <Panel title="비정상 Pods" subtitle="이름을 클릭하면 Pod 상세로 이동합니다" section={q.data?.pods} referenceIso={q.data?.generatedAt} flush>
              <SectionView section={q.data?.pods} loading={q.isLoading} emptyTitle="비정상 Pod가 없습니다">
                {(p) =>
                  p.unhealthyList.length === 0 ? (
                    <div className="state">
                      <span className="state__glyph" aria-hidden="true">
                        ✓
                      </span>
                      <span className="state__title">비정상 Pod가 없습니다</span>
                    </div>
                  ) : (
                    <table className="ds-data-table ds-data-table--compact">
                      <thead>
                        <tr>
                          <th>Pod</th>
                          <th>상태</th>
                          <th className="ds-num" style={{ width: 84 }}>
                            Restarts
                          </th>
                          <th style={{ width: 80 }}>지속</th>
                          <th style={{ width: 60 }}>로그</th>
                        </tr>
                      </thead>
                      <tbody>
                        {p.unhealthyList.map((u) => (
                          <tr key={u.name}>
                            <td className="ds-ident">
                              <Link to={refPath(u.ref, search)}>{u.name}</Link>
                            </td>
                            <td>
                              <StatusBadge severity={u.severity} label={u.reason} small />
                            </td>
                            <td className="ds-num">{u.restarts}</td>
                            <td className="num muted">{duration(u.forSeconds)}</td>
                            <td>
                              <Link to={logsPath(u.ref, search)}>열기</Link>
                            </td>
                          </tr>
                        ))}
                      </tbody>
                    </table>
                  )
                }
              </SectionView>
            </Panel>
          </div>
        </>
      )}
    </div>
  );
}
