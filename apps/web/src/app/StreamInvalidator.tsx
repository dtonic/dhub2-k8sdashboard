import { useEffect } from "react";
import { useQueryClient } from "@tanstack/react-query";
import { streamEvents } from "@/api/sse";

/**
 * SSE 신호를 재조회로 바꾸는 코얼레서. (#17)
 * - 첫 신호는 wait(기본 1s) 안에 반영합니다 — 조용한 클러스터에서 즉시성 유지.
 * - 신호가 끊이지 않아도 flush는 minInterval(기본 15s)에 한 번입니다. Pod churn이
 *   상시적인 클러스터에서 250ms마다 화면 전체를 재조회하면 컨트롤·패널이
 *   계속 출렁이고 백엔드도 연속 부하를 받습니다. 자동 갱신(기본 30s)과 같은
 *   차수의 상한을 둡니다.
 */
export function createInvalidationCoalescer(callback: () => void, wait = 1_000, minInterval = 15_000) {
  let timer: number | undefined;
  let lastFlush = Number.NEGATIVE_INFINITY;
  return {
    schedule() {
      if (timer !== undefined) return;
      const delay = Math.max(wait, lastFlush + minInterval - Date.now());
      timer = window.setTimeout(() => {
        timer = undefined;
        lastFlush = Date.now();
        callback();
      }, delay);
    },
    cancel() { if (timer !== undefined) window.clearTimeout(timer); timer = undefined; },
  };
}

export function StreamInvalidator({ clusterId }: { clusterId?: string }) {
  const client = useQueryClient();
  useEffect(() => {
	if (!clusterId) return;
	const controller = new AbortController();
	const coalescer = createInvalidationCoalescer(() => { void client.invalidateQueries({ predicate: (query) => query.queryKey.includes(clusterId) }); });
	void streamEvents(`/api/v1/clusters/${encodeURIComponent(clusterId)}/events/stream`, (message) => {
	  if (message.data || message.event === "reset") {
		window.dispatchEvent(new Event("dashboard-stream-message"));
		coalescer.schedule();
	  }
	}, controller.signal).catch(() => undefined);
	return () => { controller.abort(); coalescer.cancel(); };
  }, [client, clusterId]);
  return null;
}
