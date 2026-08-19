import { Fragment, useEffect, useRef, useState } from "react";
import { Link, useLocation, useNavigate, useSearchParams } from "react-router-dom";
import { RANGE_LABEL, type RangeKey, type TopologyNode, type TopologyNodePosition } from "@k8s-dashboard/contracts";
import { topoKeys, useEdgeSeries, useSaveTopologyLayout, useTopology } from "@/api/queries";
import { useDashboardParams } from "@/state/useDashboardParams";
import { LineChart } from "@/components/LineChart";
import { Panel, StatTile, StatusBadge } from "@/components/primitives";
import { SectionView } from "@/components/SectionState";
import { Breadcrumb, logsPath, refPath, withSearch } from "@/components/drill";
import { duration, num, since } from "@/lib/format";
import { PageError, PageHeader, useDrillControls, useInvalidate } from "@/features/drill/common";
import { useLogSearch } from "@/api/queries";
import { TopologyGraph } from "./TopologyGraph";

/**
 * Route를 클릭하면 그 통신 경로의 **출발 Pod 실제 최근 로그**를 조회해 보여줍니다. (#31 후속)
 * 실시간 패킷 캡처는 이 조회 전용 대시보드가 해선 안 되는 일이라(특권 프로세스·PII·TLS),
 * 이미 수집 중인 로그를 **실제 Pod UID**로 필터해 "이 경로에서 실제로 오간 것"에 가장
 * 가까운 실데이터를 보여줍니다. 서버 마스킹이 그대로 적용됩니다(README §10).
 *
 * route 문자열 자체(예: /api/v1/orders)는 통신 그래프가 아직 demo라 실제 로그와
 * 매칭되지 않으므로 검색어가 아니라 보조 필터로만 씁니다.
 */
function RouteLogs({
  clusterId,
  fromNode,
  route,
  range,
}: {
  clusterId: string;
  fromNode: TopologyNode | undefined;
  route: string;
  range: RangeKey;
}) {
  const external = fromNode?.external ?? false;
  const q = useLogSearch({
    clusterId,
    namespace: fromNode?.namespace || "all",
    workload: "",
    podUid: fromNode?.ref.podUid ?? "",
    container: "",
    levels: [],
    q: "",
    range,
  });
  const section = q.data?.pages[0]?.lines;
  const lines = (section?.status === "ok" ? section.data : [])?.slice(0, 8) ?? [];
  const ref = q.data?.pages[0]?.generatedAt ?? new Date().toISOString();

  if (external || !fromNode?.ref.podUid) {
    return <div className="topo-route-logs__hint muted">외부 엔티티({fromNode?.name})는 클러스터 로그가 없습니다. 내부 Pod가 출발인 경로를 선택하세요.</div>;
  }
  if (q.isLoading) return <div className="topo-route-logs__hint muted">{fromNode.name}의 최근 로그를 불러오는 중…</div>;
  if (section && section.status !== "ok") {
    return <div className="topo-route-logs__hint muted">로그를 조회할 수 없습니다({section.status}). 로그 데이터소스 연결을 확인하세요.</div>;
  }
  if (lines.length === 0) {
    return <div className="topo-route-logs__hint muted">선택한 범위에 {fromNode.name}의 로그가 없습니다.</div>;
  }
  return (
    <div className="topo-route-logs">
      <div className="topo-route-logs__meta muted">
        <strong>{fromNode.name}</strong>의 최근 로그 (route <code>{route}</code>) · Quickwit 실데이터 · 민감정보는 서버에서 마스킹됨
      </div>
      <ul className="topo-route-logs__list">
        {lines.map((l) => (
          <li key={l.id}>
            <StatusBadge severity={l.level === "ERROR" ? "critical" : l.level === "WARN" ? "warning" : "healthy"} label={l.level} small />
            <span className="topo-route-logs__pod ds-ident">{l.podName}</span>
            <span className="topo-route-logs__msg" title={l.message}>{l.message}</span>
            <span className="topo-route-logs__time num muted">{since(new Date(l.t).toISOString(), ref)}</span>
          </li>
        ))}
      </ul>
    </div>
  );
}

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

  /* Route 행을 열면 payload 표본(hex)이 펼쳐집니다. 선택한 선이 바뀌면 접습니다. (#31) */
  const [openRoute, setOpenRoute] = useState<string | null>(null);
  useEffect(() => setOpenRoute(null), [selectedEdgeId]);

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
                      <i style={{ ["--_c" as string]: "var(--color-status-serious, var(--status-critical))" }} /> 선택한 경로
                    </li>
                    <li>
                      <i style={{ ["--_c" as string]: "var(--color-border-strong)" }} /> 그 외 경로
                    </li>
                    <li>선 두께 = 트래픽 양 · 캡슐 텍스트 = 프로토콜과 누적 요청 · 상태는 노드 카드 테두리 색과 상세 뱃지로 표시</li>
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
                      {edge.routes.map((r) => {
                        const routeKey = `${r.protocol}-${r.route}`;
                        const open = openRoute === routeKey;
                        return (
                          <Fragment key={routeKey}>
                            <tr>
                              <td>{r.protocol}</td>
                              <td className="ds-ident">
                                <button
                                  type="button"
                                  className="linkish topo-route-toggle"
                                  aria-expanded={open}
                                  onClick={() => setOpenRoute(open ? null : routeKey)}
                                  title="이 경로의 최근 실제 로그 보기"
                                >
                                  {r.route}
                                </button>
                              </td>
                              <td className="ds-num">{num(r.count)}</td>
                              <td className="ds-num">{num(r.errorCount)}</td>
                            </tr>
                            {open && (
                              <tr className="topo-route-sample">
                                <td colSpan={4}>
                                  <RouteLogs
                                    clusterId={clusterId}
                                    fromNode={graph?.nodes.find((nn) => nn.id === edge.from)}
                                    route={r.route}
                                    range={range}
                                  />
                                </td>
                              </tr>
                            )}
                          </Fragment>
                        );
                      })}
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
