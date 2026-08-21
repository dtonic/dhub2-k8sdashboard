import { useEffect, useMemo, useRef } from "react";
import {
  Background,
  BackgroundVariant,
  BaseEdge,
  Controls,
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
  type ReactFlowInstance,
} from "@xyflow/react";
import "@xyflow/react/dist/style.css";
import "./topology.css";
import type { Severity, TopologyEdge, TopologyNode, TopologyNodePosition } from "@k8s-dashboard/contracts";
import { StatusDot } from "@/components/primitives";

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

/* 노드 간격을 기존의 1.5배로 늘려 캡슐(프로토콜 라벨)이 놓일 여유를 확보합니다. (#topology) */
const COL_W = 480;
const ROW_H = 198;
const EDGE_LANE_OFFSET = 16;

type PodNodeData = {
  name: string;
  namespace: string;
  severity: Severity;
  external: boolean;
  /** 이 노드로 접힌 Pod 수. 워크로드 단위 노드의 접힘 규모를 배지로 표기합니다. (#3) */
  podCount: number;
  /** 이 노드로 들어오는(수신)·나가는(송신) 프로토콜 목록. 카드에 In/Out으로 표기합니다. (#topology) */
  inProtos: string[];
  outProtos: string[];
};

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
        <span className="topo-node__ns muted">
          {d.namespace}
          {d.podCount > 1 && (
            <span className="topo-node__pods" title={`이 워크로드로 접힌 Pod ${d.podCount}개`}>
              Pod {d.podCount}
            </span>
          )}
        </span>
        {(d.inProtos.length > 0 || d.outProtos.length > 0) && (
          <span className="topo-node__protos">
            {d.inProtos.length > 0 && (
              <span className="topo-node__proto-row" title={`수신 프로토콜: ${d.inProtos.join(", ")}`}>
                <span className="topo-node__proto-dir">In</span>
                {d.inProtos.map((p) => (
                  <span key={`in-${p}`} className="topo-node__proto">
                    {p}
                  </span>
                ))}
              </span>
            )}
            {d.outProtos.length > 0 && (
              <span className="topo-node__proto-row" title={`송신 프로토콜: ${d.outProtos.join(", ")}`}>
                <span className="topo-node__proto-dir">Out</span>
                {d.outProtos.map((p) => (
                  <span key={`out-${p}`} className="topo-node__proto">
                    {p}
                  </span>
                ))}
              </span>
            )}
          </span>
        )}
      </span>
      {d.external && <span className="topo-node__external-badge">외부</span>}
      <Handle type="source" position={Position.Right} className="topo-node__handle" />
    </div>
  );
}

type TrafficEdgeData = {
  width: number;
  lane: 1 | -1;
  selected: boolean;
};

/* 선에는 더 이상 프로토콜 라벨(캡슐)을 붙이지 않습니다. 프로토콜은 노드 카드의
   In/Out 요약으로, 방향·Route·Count 상세는 하단 방향 상세 테이블로 봅니다. (#topology)
   선 자체는 ReactFlow의 클릭 히트영역으로 선택합니다(onEdgeClick). */
function TrafficEdge(props: EdgeProps) {
  const { id, sourceX, sourceY, targetX, targetY, sourcePosition, targetPosition, markerEnd } = props;
  const d = props.data as TrafficEdgeData;
  const off = EDGE_LANE_OFFSET * d.lane; // 방향별 분리 — 왕복이 한 선으로 겹치지 않게.
  const [path] = getBezierPath({
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
    <BaseEdge
      id={id}
      path={path}
      markerEnd={markerEnd}
      interactionWidth={20}
      className={d.selected ? "topo-edge topo-edge--selected" : "topo-edge"}
      style={{ stroke: color, strokeWidth: d.selected ? d.width + 0.8 : d.width }}
    />
  );
}

const nodeTypes = { pod: PodNode };
const edgeTypes = { traffic: TrafficEdge };

/**
 * 워크로드(Pod 명칭 그룹) 기준 열 배치를 계산합니다.
 * 같은 그룹의 Pod들은 **한 열에 연속으로** 쌓이고, 그룹이 많으면 여러 열에
 * 순환 배치해 화면이 한 줄로 길어지지 않게 합니다. 열 수는 그룹 수의 √n에
 * 비례해 늘려(최소 4) 대형 클러스터에서 한 열이 끝없이 길어지지 않게 합니다.
 * 입력이 같으면 배치도 같아 갱신 때 노드가 튀지 않습니다. (#3)
 */
const MIN_COLS = 4;
const GROUP_GAP = 90;

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

  const maxCols = Math.max(MIN_COLS, Math.ceil(Math.sqrt(families.length)));
  const colHeights = Array.from({ length: maxCols }, () => 0);
  let col = 0;
  for (const family of families) {
    const members = byFamily.get(family)!.filter((n) => !n.external);
    if (members.length === 0) continue;
    const c = col % maxCols;
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

  /* fitView는 React Flow가 최초 마운트에만 적용합니다. namespace 전환처럼 노드
     **구성**(id 집합)이 바뀌면 이전 뷰포트(전체 뷰에 맞춘 축소/이동 상태)가 남아
     부분집합이 화면 밖에 놓입니다 — 구성이 바뀔 때만 다시 fit합니다. 같은 구성의
     자동 갱신은 사용자의 팬/줌을 유지하고, 편집 모드 중에는 건드리지 않습니다. (#16) */
  const flowRef = useRef<ReactFlowInstance | null>(null);
  const nodeSetKey = useMemo(() => graphNodes.map((n) => n.id).sort().join("|"), [graphNodes]);
  const fittedKeyRef = useRef<string | null>(null);
  useEffect(() => {
    if (!flowRef.current || editRef.current) return;
    if (fittedKeyRef.current === null || fittedKeyRef.current === nodeSetKey) return;
    fittedKeyRef.current = nodeSetKey;
    /* setNodes 반영 이후에 fit해야 새 노드 좌표 기준으로 잡힙니다. */
    const t = window.setTimeout(() => {
      void flowRef.current?.fitView({ padding: 0.15, maxZoom: 1 });
    }, 60);
    return () => window.clearTimeout(t);
  }, [nodeSetKey]);

  /* 노드별 In/Out 프로토콜 집계. from 엣지 = 송신(Out), to 엣지 = 수신(In).
     프로토콜은 고정 순서(TCP·UDP·HTTP·gRPC)로 정렬해 카드마다 일관되게 보입니다. */
  const protoByNode = useMemo(() => {
    const order = ["HTTP", "gRPC", "TCP", "UDP"];
    const rank = (p: string) => {
      const i = order.indexOf(p);
      return i < 0 ? order.length : i;
    };
    const map = new Map<string, { in: Set<string>; out: Set<string> }>();
    const slot = (id: string) => {
      let s = map.get(id);
      if (!s) {
        s = { in: new Set(), out: new Set() };
        map.set(id, s);
      }
      return s;
    };
    for (const e of graphEdges) {
      for (const p of e.protocols) {
        slot(e.from).out.add(p);
        slot(e.to).in.add(p);
      }
    }
    const sorted = (s: Set<string>) => [...s].sort((a, b) => rank(a) - rank(b));
    return { get: (id: string) => map.get(id), sorted };
  }, [graphEdges]);
  const protoKey = useMemo(
    () => graphEdges.map((e) => `${e.from}>${e.to}:${e.protocols.join(",")}`).join("|"),
    [graphEdges],
  );

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
        data: {
          name: n.name,
          namespace: n.namespace,
          severity: n.severity,
          external: n.external ?? false,
          podCount: n.podCount ?? 1,
          inProtos: protoByNode.get(n.id) ? protoByNode.sorted(protoByNode.get(n.id)!.in) : [],
          outProtos: protoByNode.get(n.id) ? protoByNode.sorted(protoByNode.get(n.id)!.out) : [],
        } satisfies PodNodeData,
        draggable: editMode,
        connectable: false,
      }));
      posRef.current = new Map(next.map((n) => [n.id, n.position]));
      return next;
    });
    /* 편집 모드에서는 드래그 전에도 부모가 현재 좌표를 알아야 "저장"이 가능합니다. */
    if (editRef.current) emitPositions();
    // eslint-disable-next-line react-hooks/exhaustive-deps -- savedPositions·protoByNode는 savedKey·protoKey로 비교합니다.
  }, [graphNodes, savedKey, protoKey, editMode, editSession, setNodes]);

  /* 엣지 동기화 — 두께는 트래픽 양을 최댓값 대비로 정규화합니다. */
  useEffect(() => {
    const maxCount = Math.max(1, ...graphEdges.map((e) => e.totalCount));
    setEdges(
      graphEdges.map((e) => {
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
            /* 두께 = 트래픽 양. sqrt로 눌러 1~2.2px 범위에 둔다. (#31) */
            width: 1 + 1.2 * Math.sqrt(e.totalCount / maxCount),
            lane: (e.from < e.to ? 1 : -1) as 1 | -1,
            selected,
          } satisfies TrafficEdgeData,
        };
      }),
    );
  }, [graphEdges, selectedEdgeId, setEdges]);

  return (
    <div className={editMode ? "topo-canvas topo-canvas--edit" : "topo-canvas"}>
      <ReactFlow
        nodes={nodes}
        edges={edges}
        nodeTypes={nodeTypes}
        edgeTypes={edgeTypes}
        onInit={(instance) => {
          flowRef.current = instance;
          /* 최초 마운트는 fitView prop이 처리합니다 — 여기서는 기준 구성만 기록합니다. */
          fittedKeyRef.current = nodeSetKey;
        }}
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
