import { useEffect } from "react";
import { useQueryClient } from "@tanstack/react-query";
import { streamEvents } from "@/api/sse";

export function createInvalidationCoalescer(callback: () => void, wait = 250) {
  let timer: number | undefined;
  return {
    schedule() { if (timer === undefined) timer = window.setTimeout(() => { timer = undefined; callback(); }, wait); },
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
