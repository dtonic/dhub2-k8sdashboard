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

type PodNodeData = { name: string; namespace: string; severity: Severity };

/* 상태 색은 예약된 상태 토큰만 씁니다 — severity마다 전용 토큰이 있습니다. */
function severityVar(s: Severity): string {
  switch (s) {
    case "critical":
      return "var(--status-critical)";
    case "warning":
      return "var(--status-warning)";
    case "degraded":
      return "var(--status-degraded)";
    case "progressing":
      return "var(--status-progressing)";
    case "healthy":
      return "var(--status-healthy)";
    default:
      return "var(--status-unknown)";
  }
}

function PodNode({ data }: NodeProps) {
  const d = data as PodNodeData;
  return (
    <div className={`topo-node topo-node--${d.severity}`}>
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
  const color = severityVar(d.severity);
  return (
    <>
      <BaseEdge
        id={id}
        path={path}
        markerEnd={markerEnd}
        className={d.selected ? "topo-edge topo-edge--selected" : "topo-edge"}
        style={{ stroke: color, strokeWidth: d.selected ? d.width + 1.2 : d.width }}
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
  const colHeights = Array.from({ length: MAX_COLS }, () => 0);
  families.forEach((family, i) => {
    const col = i % MAX_COLS;
    const members = byFamily.get(family)!;
    members.forEach((n, row) => {
      pos.set(n.id, { x: col * COL_W, y: colHeights[col]! + row * ROW_H });
    });
    colHeights[col]! += members.length * ROW_H + GROUP_GAP;
  });
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
        data: { name: n.name, namespace: n.namespace, severity: n.severity } satisfies PodNodeData,
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
        return {
          id: e.id,
          source: e.from,
          target: e.to,
          type: "traffic",
          markerEnd: { type: MarkerType.ArrowClosed, width: 14, height: 14, color: severityVar(severity) },
          data: {
            proto: e.protocols.join("/"),
            total: e.totalCount,
            severity,
            width: 1.5 + 3.5 * (e.totalCount / maxCount),
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
          if (n) onSelectNode(n);
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
