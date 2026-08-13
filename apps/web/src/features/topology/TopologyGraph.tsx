import { useMemo } from "react";
import type { TopologyEdge, TopologyNode } from "@k8s-dashboard/contracts";
import { compact, num } from "@/lib/format";

/**
 * Pod Topology 그래프 (design-system `components/topology` 규칙을 그대로 구현)
 * --------------------------------------------------------------------------
 * - `A→B`와 `B→A`는 **서로 다른 선**입니다. 노드 중심선에서 각각 10px씩 반대편으로
 *   밀어 평행하게 그립니다(총 20px 분리). 방향마다 클릭 타겟과 상세 데이터가 다릅니다.
 * - 라벨(방향 캡슐)은 선보다 폭이 넓어 오프셋만으로는 겹칩니다. 선 위 후보 지점 중
 *   빈 자리를 골라 배치하고, 캡슐만 별도 레이어로 모든 선 위에 그립니다.
 * - 색은 **상태**, 두께는 트래픽 양, 프로토콜은 캡슐 텍스트입니다.
 *   색상 채널을 상태에 예약해야 "빨간 선 = 문제 경로"가 화면 어디서나 참이 됩니다.
 * - 각 선은 tabindex를 가진 버튼입니다. 키보드로 순회하고 Enter로 선택합니다.
 */

const NODE_W = 200;
const NODE_H = 56;
const EDGE_OFFSET = 10;
const ARROW = 9;
const VB_W = 1280;
const VB_H = 470;

const CAP_FRACTIONS = [0.38, 0.3, 0.46, 0.24, 0.54, 0.18, 0.62, 0.12];

type Box = { x1: number; y1: number; x2: number; y2: number };

/* 열 간격은 노드 폭 + 캡슐이 들어갈 여유(120px)를 확보합니다.
   여유가 없으면 캡슐이 노드 위로 밀려 이름을 가립니다. */
const posOf = (n: TopologyNode) => ({ x: 120 + n.column * 320, y: 95 + n.row * 145 });

export function TopologyGraph({
  nodes,
  edges,
  selectedEdgeId,
  onSelectEdge,
  onSelectNode,
}: {
  nodes: TopologyNode[];
  edges: TopologyEdge[];
  selectedEdgeId: string | null;
  onSelectEdge: (id: string) => void;
  onSelectNode: (node: TopologyNode) => void;
}) {
  const layout = useMemo(() => {
    const byId = Object.fromEntries(nodes.map((n) => [n.id, n]));
    /* 노드 박스를 먼저 점유 목록에 넣습니다. 캡슐이 노드 위에 얹히면 이름을 가립니다. */
    const placed: Box[] = nodes.map((n) => {
      const p = posOf(n);
      return { x1: p.x - NODE_W / 2 - 4, y1: p.y - NODE_H / 2 - 4, x2: p.x + NODE_W / 2 + 4, y2: p.y + NODE_H / 2 + 4 };
    });

    const items = edges.map((e) => {
      const a = posOf(byId[e.from]!);
      const b = posOf(byId[e.to]!);
      const dx = b.x - a.x;
      const dy = b.y - a.y;
      const len = Math.max(1, Math.hypot(dx, dy));
      const ux = dx / len;
      const uy = dy / len;
      const px = -uy;
      const py = ux;
      const hw = NODE_W / 2 + 8;
      const hh = NODE_H / 2 + 8;
      const t = Math.min(
        Math.abs(ux) < 1e-6 ? Infinity : hw / Math.abs(ux),
        Math.abs(uy) < 1e-6 ? Infinity : hh / Math.abs(uy),
      );
      const ox = px * EDGE_OFFSET;
      const oy = py * EDGE_OFFSET;
      const x1 = a.x + ux * t + ox;
      const y1 = a.y + uy * t + oy;
      const x2 = b.x - ux * (t + ARROW) + ox;
      const y2 = b.y - uy * (t + ARROW) + oy;
      const tipX = b.x - ux * t + ox;
      const tipY = b.y - uy * t + oy;

      const proto = e.protocols.join("/");
      /* 캡슐은 좁습니다. 정확한 수치는 아래 상세 표에 있으므로 축약합니다. */
      const capText = `${proto} ${compact(e.totalCount)}`;
      const capW = capText.length * 5.6 + 14;

      /* 라벨 충돌 회피 — 빈 자리를 찾고, 없으면 **가장 덜 겹치는** 자리를 고릅니다.
         첫 후보로 되돌아가면 짧은 수평 선에서 라벨이 노드 이름을 가립니다. */
      let cap = { cx: x1 + (x2 - x1) * 0.38, cy: y1 + (y2 - y1) * 0.38 };
      let capBox: Box | null = null;
      let bestArea = Infinity;
      for (const f of CAP_FRACTIONS) {
        const cx = x1 + (x2 - x1) * f;
        const cy = y1 + (y2 - y1) * f;
        const box: Box = { x1: cx - capW / 2 - 4, y1: cy - 13, x2: cx + capW / 2 + 4, y2: cy + 13 };
        const area = placed.reduce((sum, q) => {
          const w = Math.min(box.x2, q.x2) - Math.max(box.x1, q.x1);
          const h = Math.min(box.y2, q.y2) - Math.max(box.y1, q.y1);
          return sum + (w > 0 && h > 0 ? w * h : 0);
        }, 0);
        if (area < bestArea) {
          bestArea = area;
          cap = { cx, cy };
          capBox = box;
        }
        if (area === 0) break;
      }
      if (capBox) placed.push(capBox);

      const w = 4.5;
      const bx = tipX - ux * ARROW;
      const by = tipY - uy * ARROW;
      const arrow = `M ${tipX} ${tipY} L ${bx + px * w} ${by + py * w} L ${bx - px * w} ${by - py * w} Z`;
      const stroke = Math.max(2, Math.min(6, 2 + Math.log10(e.totalCount / 40000 + 1) * 3));

      return { e, x1, y1, x2, y2, arrow, cap, capText, capW, stroke, proto };
    });

    return items;
  }, [nodes, edges]);

  const selected = edges.find((e) => e.id === selectedEdgeId) ?? null;
  const active = new Set(selected ? [selected.from, selected.to] : []);

  return (
    <div className="ds-topology__canvas">
      <svg
        className="ds-topology__svg"
        viewBox={`0 0 ${VB_W} ${VB_H}`}
        role="application"
        aria-label="Pod 간 통신 토폴로지. 선을 선택하면 방향별 요청 상세를 볼 수 있습니다."
      >
        {/* 선 레이어 */}
        {layout.map((l) => {
          const dim = selected && l.e.id !== selected.id ? " ds-topo-edge--dimmed" : "";
          const on = selected && l.e.id === selected.id ? " ds-topo-edge--selected" : "";
          return (
            <g
              key={l.e.id}
              className={`ds-topo-edge ds-topo-edge--${l.e.severity}${dim}${on}`}
              tabIndex={0}
              role="button"
              aria-label={`${l.e.from}에서 ${l.e.to}로 ${l.proto} 요청 ${num(l.e.totalCount)}건, 에러율 ${(l.e.errorRate * 100).toFixed(1)}%`}
              onClick={() => onSelectEdge(l.e.id)}
              onKeyDown={(ev) => {
                if (ev.key === "Enter" || ev.key === " ") {
                  ev.preventDefault();
                  onSelectEdge(l.e.id);
                }
              }}
            >
              <line className="ds-topo-edge__hit" x1={l.x1} y1={l.y1} x2={l.x2} y2={l.y2} />
              <line className="ds-topo-edge__line" x1={l.x1} y1={l.y1} x2={l.x2} y2={l.y2} strokeWidth={l.stroke} />
              <path className="ds-topo-edge__arrow" d={l.arrow} />
            </g>
          );
        })}

        {/* 캡슐 레이어 — 모든 선 위에 그립니다 */}
        {layout.map((l) => {
          const dim = selected && l.e.id !== selected.id ? " ds-topo-edge--dimmed" : "";
          return (
            <g
              key={`cap-${l.e.id}`}
              className={`ds-topo-cap ds-topo-edge--${l.e.severity}${dim}`}
              aria-hidden="true"
              onClick={() => onSelectEdge(l.e.id)}
            >
              <rect className="ds-topo-edge__cap" x={l.cap.cx - l.capW / 2} y={l.cap.cy - 9} width={l.capW} height={18} />
              <text className="ds-topo-edge__cap-text" x={l.cap.cx} y={l.cap.cy + 3.5} textAnchor="middle">
                {l.capText}
              </text>
            </g>
          );
        })}

        {/* 노드 레이어 */}
        {nodes.map((n) => {
          const p = posOf(n);
          const x = p.x - NODE_W / 2;
          const y = p.y - NODE_H / 2;
          const dim = selected && !active.has(n.id) ? " ds-topo-node--dimmed" : "";
          return (
            <g
              key={n.id}
              className={`ds-topo-node ds-topo-node--${n.severity}${dim}`}
              tabIndex={0}
              role="button"
              aria-label={`${n.name}, 상태 ${n.severity}. Pod 상세로 이동`}
              onClick={() => onSelectNode(n)}
              onKeyDown={(ev) => {
                if (ev.key === "Enter" || ev.key === " ") {
                  ev.preventDefault();
                  onSelectNode(n);
                }
              }}
            >
              <rect className="ds-topo-node__box" x={x} y={y} width={NODE_W} height={NODE_H} rx={8} />
              <rect className="ds-topo-node__rail" x={x + 5} y={y + 10} width={4} height={NODE_H - 20} rx={2} />
              <text className="ds-topo-node__name" x={x + 18} y={y + 24}>
                {n.name.length > 27 ? `${n.name.slice(0, 26)}…` : n.name}
                <title>{n.name}</title>
              </text>
              <text className="ds-topo-node__meta" x={x + 18} y={y + 41}>
                {n.namespace} · {n.severity}
              </text>
            </g>
          );
        })}
      </svg>
    </div>
  );
}
