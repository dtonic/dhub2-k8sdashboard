import { create } from "zustand";
import type { RangeKey } from "@k8s-dashboard/contracts";

/**
 * 전역 상태 스토어 (zustand) — **URL이 유일한 진실 원천**입니다.
 * --------------------------------------------------------------------------
 * 필터·선택을 URL에 두는 규칙(CLAUDE.md)은 그대로입니다. 이 스토어는 URL의
 * query parameter(cluster·ns·range·refresh)와 path parameter(pod 이름 등)를
 * 미러링하는 읽기 사본으로, 라우터 훅을 쓸 수 없는 코드도 현재 Scope를
 * 일관되게 보게 합니다.
 *
 * 동기화 규칙: 스토어 값과 URL 값이 다르면 **항상 URL 값으로 스토어를
 * 덮어씁니다.** 반대 방향(스토어 → URL)의 쓰기는 없습니다 — 변경은
 * `useDashboardParams().patch` → URL 갱신 → sync 순서로만 흐릅니다.
 * 동기화는 useDashboardParams가 렌더마다 수행하므로 두 값이 어긋난 상태로
 * 유지될 수 없습니다.
 */

export interface DashboardParamsState {
  clusterId: string;
  namespace: string;
  range: RangeKey;
  refreshMs: number;
  /** 현재 라우트의 path parameter (예: /pods/{name}의 name). */
  pathParams: Record<string, string | undefined>;
  /** URL에서 읽은 값으로 스토어를 맞춥니다. 값이 같으면 아무것도 하지 않습니다. */
  syncFromUrl(next: Omit<DashboardParamsState, "syncFromUrl">): void;
}

function sameRecord(a: Record<string, string | undefined>, b: Record<string, string | undefined>) {
  const ak = Object.keys(a);
  const bk = Object.keys(b);
  if (ak.length !== bk.length) return false;
  return ak.every((k) => a[k] === b[k]);
}

export const useDashboardStore = create<DashboardParamsState>((set, get) => ({
  clusterId: "",
  namespace: "all",
  range: "1h",
  refreshMs: 30_000,
  pathParams: {},
  syncFromUrl(next) {
    const cur = get();
    if (
      cur.clusterId === next.clusterId &&
      cur.namespace === next.namespace &&
      cur.range === next.range &&
      cur.refreshMs === next.refreshMs &&
      sameRecord(cur.pathParams, next.pathParams)
    ) {
      return;
    }
    set(next);
  },
}));
