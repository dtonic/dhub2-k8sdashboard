import type { NodePodSummary, NodeSummary, Severity } from "@k8s-dashboard/contracts";
import { podsOf, workloadsOf } from "./drilldown";

/**
 * Nodes 화면 mock — Pod 신원(이름·UID·노드 배치)은 drilldown 픽스처에서
 * 빌려옵니다. 여기서 새 이름을 지어내면 Pod 상세로 가는 deep link가 404가 됩니다.
 */
export function nodeSummaries(allowed: string[]): NodeSummary[] {
  const byNode = new Map<string, NodePodSummary[]>();
  for (const ns of allowed) {
    for (const w of workloadsOf(ns)) {
      for (const p of podsOf(w)) {
        if (p.finishedAt) continue; // 종료된 인스턴스는 스케줄 자원을 차지하지 않습니다
        const rows = byNode.get(p.node) ?? [];
        rows.push({
          uid: p.uid,
          name: p.name,
          namespace: p.namespace,
          phase: p.phase,
          severity: p.severity,
          restarts: p.restarts,
          cpuRequestMilli: p.usage.cpuRequestMilli,
          memoryRequestMib: p.usage.memoryRequestMib,
        });
        byNode.set(p.node, rows);
      }
    }
  }

  const names = [...byNode.keys()].sort();
  return names.map((name, i) => {
    const pods = byNode.get(name) ?? [];
    const requested = pods.reduce(
      (acc, p) => ({ cpuMilli: acc.cpuMilli + p.cpuRequestMilli, memoryMib: acc.memoryMib + p.memoryRequestMib }),
      { cpuMilli: 0, memoryMib: 0 },
    );
    // 마지막 노드 하나는 메모리 압박 상태로 두어 상태 구분이 렌더되는지 확인합니다.
    const pressure = i === names.length - 1 && names.length > 1;
    const severity: Severity = pressure ? "degraded" : "healthy";
    return {
      name,
      roles: i === 0 ? ["control-plane", "master"] : ["worker"],
      ready: true,
      unschedulable: false,
      pressure,
      severity,
      kubeletVersion: "v1.31.4",
      osImage: "Fedora CoreOS 41",
      internalIP: `10.0.${i}.10`,
      ageSeconds: 86_400 * (30 + i * 7),
      capacity: { cpuMilli: 8_000, memoryMib: 16_384, pods: 250 },
      allocatable: { cpuMilli: 7_500, memoryMib: 14_800, pods: 250 },
      requested,
      limits: { cpuMilli: Math.round(requested.cpuMilli * 1.5), memoryMib: Math.round(requested.memoryMib * 1.4) },
      podsTotal: pods.length,
      pods,
    };
  });
}
