import { useEffect, useMemo, useRef } from "react";
import {
  Background,
  BackgroundVariant,
  BaseEdge,
  Controls,
  EdgeLabelRenderer,
  getBezierPath,
  Handle,
  MarkerType,
  MiniMap,
  Position,
  ReactFlow,
  useEdgesState,
  useNodesState,
  type Edge,
  type EdgeProps,
  type Node,
  type NodeProps,
} from "@xyflow/react";
import "@xyflow/react/dist/style.css";
import "./topology.css";
import type { Severity, TopologyEdge, TopologyNode, TopologyNodePosition } from "@k8s-dashboard/contracts";
import { StatusDot } from "@/components/primitives";
import { compact } from "@/lib/format";

/**
 * Pod Topology 그래프 — React Flow 기반. (#28)
 * --------------------------------------------------------------------------
 * design-system topology 규칙은 렌더러가 바뀌어도 그대로입니다:
 * - `A→B`와 `B→A`는 **서로 다른 선**입니다. 방향별로 곡선을 반대편으로 offset해
 *   두 개의 평행한 베지어로 그립니다.
 * - 선 색 = 상태(예약된 상태 토큰), 두께 = 트래픽 양, 프로토콜 = 캡슐 텍스트.
 * - 캡슐과 선은 클릭/키보드로 선택할 수 있고 선택된 방향의 상세가 열립니다.
 *
 * 기본 배치는 **Pod 명칭(워크로드) 그룹별 세로 열**입니다 — 같은 워크로드의
 * Pod들이 한 열을 이루고, 열과 열 사이를 곡선이 잇습니다. 관리자가 저장한
 * 공유 배치(layout)가 있으면 그 좌표가 기본값을 덮습니다.
 *
 * 자동 갱신은 데이터만 바꿉니다 — 편집 중 드래그한 좌표는 서버 갱신이
 * 지우지 않습니다. (CLAUDE.md: 사용자 상태를 갱신이 지우지 않습니다)
 */

const COL_W = 320;
const ROW_H = 132;
const EDGE_LANE_OFFSET = 14;

type PodNodeData = { name: string; namespace: string; severity: Severity; external: boolean };

/* 선 색 규칙 (#31, 사용자 결정): 기본 선은 중립색 하나로 통일해 화면을 차분하게
   유지하고, **선택한 선만** 주황~빨강 계열 강조 토큰으로 집중시킵니다.
   상태(severity)는 노드 카드의 왼쪽 테두리·상태 점과 방향 상세의 뱃지가 전달합니다.
   (design-system의 "선 색=상태" 규칙을 사용자 지시로 대체한 결정 — Issue #31) */
const EDGE_COLOR_DEFAULT = "var(--color-border-strong)";
const EDGE_COLOR_SELECTED = "var(--color-status-serious, var(--status-critical))";

function PodNode({ data }: NodeProps) {
  const d = data as PodNodeData;
  return (
    <div className={`topo-node topo-node--${d.severity}${d.external ? " topo-node--external" : ""}`}>
      <Handle type="target" position={Position.Left} className="topo-node__handle" />
      <span className="topo-node__status">
        <StatusDot severity={d.severity} />
      </span>
      <span className="topo-node__body">
        <span className="topo-node__name ds-ident" title={d.name}>
          {d.name}
        </span>
        <span className="topo-node__ns muted">{d.namespace}</span>
      </span>
      {d.external && <span className="topo-node__external-badge">외부</span>}
      <Handle type="source" position={Position.Right} className="topo-node__handle" />
    </div>
  );
}

type TrafficEdgeData = {
  proto: string;
  total: number;
  severity: Severity;
  width: number;
  lane: 1 | -1;
  /** 캡슐이 서로 겹치지 않도록 곡선 위 위치를 흩뿌리는 지수. */
  stagger: number;
  fromName: string;
  toName: string;
  selected: boolean;
  onSelect: (id: string) => void;
};

function TrafficEdge(props: EdgeProps) {
  const { id, sourceX, sourceY, targetX, targetY, sourcePosition, targetPosition, markerEnd } = props;
  const d = props.data as TrafficEdgeData;
  /* 방향별 lane offset — 같은 두 노드 사이의 왕복이 한 선으로 겹치지 않습니다. */
  const off = EDGE_LANE_OFFSET * d.lane;
  const [path, labelX, labelY] = getBezierPath({
    sourceX,
    sourceY: sourceY + off,
    targetX,
    targetY: targetY + off,
    sourcePosition,
    targetPosition,
    curvature: 0.4,
  });
  const color = d.selected ? EDGE_COLOR_SELECTED : EDGE_COLOR_DEFAULT;
  return (
    <>
      <BaseEdge
        id={id}
        path={path}
        markerEnd={markerEnd}
        className={d.selected ? "topo-edge topo-edge--selected" : "topo-edge"}
        style={{ stroke: color, strokeWidth: d.selected ? d.width + 0.8 : d.width }}
      />
      <EdgeLabelRenderer>
        <button
          type="button"
          className={d.selected ? "topo-edge__cap topo-edge__cap--selected" : "topo-edge__cap"}
          style={{ transform: `translate(-50%, -50%) translate(${labelX + d.stagger}px, ${labelY}px)` }}
          onClick={() => d.onSelect(id)}
          aria-pressed={d.selected}
          aria-label={`${d.fromName}에서 ${d.toName}로 가는 ${d.proto} 경로 선택 · 누적 ${d.total}건`}
        >
          {d.proto} {compact(d.total)}
        </button>
      </EdgeLabelRenderer>
    </>
  );
}

const nodeTypes = { pod: PodNode };
const edgeTypes = { traffic: TrafficEdge };

/**
 * 워크로드(Pod 명칭 그룹) 기준 열 배치를 계산합니다.
 * 같은 그룹의 Pod들은 **한 열에 연속으로** 쌓이고, 그룹이 많으면 4개 열에
 * 순환 배치해 화면이 한 줄로 길어지지 않게 합니다(서버 기본 열 수와 동일).
 */
const MAX_COLS = 4;
const GROUP_GAP = 60;

function defaultPositions(nodes: TopologyNode[]): Map<string, { x: number; y: number }> {
  const families: string[] = [];
  const byFamily = new Map<string, TopologyNode[]>();
  for (const n of nodes) {
    const family = `${n.namespace}/${n.ref.workloadName || n.name}`;
    if (!byFamily.has(family)) {
      byFamily.set(family, []);
      families.push(family);
    }
    byFamily.get(family)!.push(n);
  }
  const pos = new Map<string, { x: number; y: number }>();
  /* 외부 엔티티(Client·Gateway·External API)는 맨 왼쪽 전용 열에 세로로 쌓습니다.
     내부 워크로드 열은 한 칸 오른쪽부터 시작합니다. (#29) */
  const externals = nodes.filter((n) => n.external);
  externals.forEach((n, row) => pos.set(n.id, { x: 0, y: row * (ROW_H + GROUP_GAP) }));

  const colHeights = Array.from({ length: MAX_COLS }, () => 0);
  let col = 0;
  for (const family of families) {
    const members = byFamily.get(family)!.filter((n) => !n.external);
    if (members.length === 0) continue;
    const c = col % MAX_COLS;
    members.forEach((n, row) => {
      pos.set(n.id, { x: (1 + c) * COL_W, y: colHeights[c]! + row * ROW_H });
    });
    colHeights[c]! += members.length * ROW_H + GROUP_GAP;
    col++;
  }
  return pos;
}

export function TopologyGraph({
  nodes: graphNodes,
  edges: graphEdges,
  savedPositions,
  selectedEdgeId,
  editMode,
  editSession,
  onSelectEdge,
  onSelectNode,
  onPositionsChange,
}: {
  nodes: TopologyNode[];
  edges: TopologyEdge[];
  /** 서버에 저장된 공유 배치. 기본 열 배치보다 우선합니다. */
  savedPositions: TopologyNodePosition[] | null;
  selectedEdgeId: string | null;
  /** true면 노드를 드래그할 수 있고, 서버 갱신이 좌표를 덮지 않습니다. */
  editMode: boolean;
  /** 값이 바뀌면 편집 중 좌표를 버리고 저장/기본 배치로 되돌립니다(취소). */
  editSession: number;
  onSelectEdge: (id: string) => void;
  onSelectNode: (node: TopologyNode) => void;
  onPositionsChange?: (positions: TopologyNodePosition[]) => void;
}) {
  const [nodes, setNodes, onNodesChange] = useNodesState<Node>([]);
  const [edges, setEdges] = useEdgesState<Edge>([]);
  const byId = useMemo(() => new Map(graphNodes.map((n) => [n.id, n])), [graphNodes]);
  const editRef = useRef(editMode);
  editRef.current = editMode;
  /* 현재 좌표의 사본 — 저장 시점에 부모가 최신 좌표를 받도록 drag stop마다 갱신합니다. */
  const posRef = useRef(new Map<string, { x: number; y: number }>());

  const savedKey = useMemo(() => JSON.stringify(savedPositions ?? []), [savedPositions]);

  const emitPositions = () => {
    onPositionsChange?.(
      Array.from(posRef.current, ([id, p]) => ({ id, x: Math.round(p.x), y: Math.round(p.y) })),
    );
  };

  /* 노드 동기화 — 편집 중에는 기존 좌표를 유지하고, 조회 모드에서는
     저장 배치 → 기본 열 배치 순서로 서버가 이깁니다. */
  useEffect(() => {
    const saved = new Map((savedPositions ?? []).map((p) => [p.id, { x: p.x, y: p.y }]));
    const defaults = defaultPositions(graphNodes);
    setNodes((prev) => {
      const prevPos = new Map(prev.map((p) => [p.id, p.position]));
      const next = graphNodes.map((n) => ({
        id: n.id,
        type: "pod",
        position:
          (editRef.current ? prevPos.get(n.id) : undefined) ??
          saved.get(n.id) ??
          defaults.get(n.id) ?? { x: 0, y: 0 },
        data: { name: n.name, namespace: n.namespace, severity: n.severity, external: n.external ?? false } satisfies PodNodeData,
        draggable: editMode,
        connectable: false,
      }));
      posRef.current = new Map(next.map((n) => [n.id, n.position]));
      return next;
    });
    /* 편집 모드에서는 드래그 전에도 부모가 현재 좌표를 알아야 "저장"이 가능합니다. */
    if (editRef.current) emitPositions();
    // eslint-disable-next-line react-hooks/exhaustive-deps -- savedPositions는 savedKey로 비교합니다.
  }, [graphNodes, savedKey, editMode, editSession, setNodes]);

  /* 엣지 동기화 — 두께는 트래픽 양을 최댓값 대비로 정규화합니다. */
  useEffect(() => {
    const maxCount = Math.max(1, ...graphEdges.map((e) => e.totalCount));
    setEdges(
      graphEdges.map((e, i) => {
        const severity = e.severity;
        const selected = e.id === selectedEdgeId;
        return {
          id: e.id,
          source: e.from,
          target: e.to,
          type: "traffic",
          markerEnd: {
            type: MarkerType.ArrowClosed,
            width: 13,
            height: 13,
            color: selected ? "var(--color-status-serious, var(--status-critical))" : "var(--color-border-strong)",
          },
          data: {
            proto: e.protocols.join("/"),
            total: e.totalCount,
            severity,
            /* 두께 = 트래픽 양. 선형 스케일은 최대 5px까지 굵어져 난잡했다 —
               sqrt로 눌러 1~2.2px 범위에 둔다. (#31) */
            width: 1 + 1.2 * Math.sqrt(e.totalCount / maxCount),
            lane: (e.from < e.to ? 1 : -1) as 1 | -1,
            stagger: ((i % 5) - 2) * 30,
            fromName: byId.get(e.from)?.name ?? e.from,
            toName: byId.get(e.to)?.name ?? e.to,
            selected: e.id === selectedEdgeId,
            onSelect: onSelectEdge,
          } satisfies TrafficEdgeData,
        };
      }),
    );
  }, [graphEdges, byId, selectedEdgeId, onSelectEdge, setEdges]);

  return (
    <div className={editMode ? "topo-canvas topo-canvas--edit" : "topo-canvas"}>
      <ReactFlow
        nodes={nodes}
        edges={edges}
        nodeTypes={nodeTypes}
        edgeTypes={edgeTypes}
        onNodesChange={onNodesChange}
        onNodeDragStop={(_, __, dragged) => {
          for (const n of dragged) posRef.current.set(n.id, n.position);
          emitPositions();
        }}
        onNodeClick={(_, node) => {
          if (editMode) return;
          const n = byId.get(node.id);
          /* 외부 엔티티는 Pod 신원이 없으므로 상세 화면으로 보내지 않습니다. (#29) */
          if (n && !n.external) onSelectNode(n);
        }}
        onEdgeClick={(_, edge) => onSelectEdge(edge.id)}
        nodesDraggable={editMode}
        nodesConnectable={false}
        elementsSelectable
        fitView
        fitViewOptions={{ padding: 0.15, maxZoom: 1 }}
        minZoom={0.2}
        maxZoom={2}
        proOptions={{ hideAttribution: true }}
        aria-label="Pod 통신 그래프. 선을 클릭하면 방향별 상세가 열립니다."
      >
        <Background variant={BackgroundVariant.Dots} gap={22} size={1.2} className="topo-canvas__bg" />
        <Controls showInteractive={false} position="bottom-left" />
        <MiniMap pannable zoomable position="bottom-right" nodeStrokeWidth={2} className="topo-canvas__minimap" />
      </ReactFlow>
    </div>
  );
}
