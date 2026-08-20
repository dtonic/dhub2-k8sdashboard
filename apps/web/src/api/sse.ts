import { getManagerToken, refreshAuth } from "./client";

export type SSEMessage = { id: string; event: string; data: string; retry?: number };
const MAX_LINE = 8 * 1024, MAX_DATA = 64 * 1024, MAX_BUFFER = 128 * 1024;

export class SSEParser {
  private pending = new Uint8Array(); private data: string[] = []; private dataBytes = 0;
  private id = ""; private event = "message"; private retry: number | undefined;
  private readonly decoder = new TextDecoder("utf-8", { fatal: true });

  push(chunk: Uint8Array, emit: (message: SSEMessage) => void) {
    if (this.pending.byteLength + chunk.byteLength > MAX_BUFFER) throw new Error("SSE buffer exceeded");
    const joined = new Uint8Array(this.pending.byteLength + chunk.byteLength); joined.set(this.pending); joined.set(chunk, this.pending.byteLength);
    let start = 0;
    for (let index = 0; index < joined.byteLength; index++) {
      if (joined[index] !== 0x0a) continue;
      let end = index; if (end > start && joined[end - 1] === 0x0d) end--;
      const lineBytes = joined.subarray(start, end); if (lineBytes.byteLength > MAX_LINE) throw new Error("SSE line exceeded");
      this.line(this.decoder.decode(lineBytes), lineBytes.byteLength, emit); start = index + 1;
    }
    this.pending = joined.slice(start); if (this.pending.byteLength > MAX_LINE) throw new Error("SSE line exceeded");
  }

  finish(emit: (message: SSEMessage) => void) {
    if (this.pending.byteLength) { this.line(this.decoder.decode(this.pending), this.pending.byteLength, emit); this.pending = new Uint8Array(); }
    this.dispatch(emit);
  }

  private line(line: string, wireBytes: number, emit: (message: SSEMessage) => void) {
    if (line === "") { this.dispatch(emit); return; } if (line.startsWith(":")) return;
    const colon = line.indexOf(":"); const field = colon < 0 ? line : line.slice(0, colon);
    let value = colon < 0 ? "" : line.slice(colon + 1); if (value.startsWith(" ")) value = value.slice(1);
    if (field === "data") { this.dataBytes += wireBytes; if (this.dataBytes > MAX_DATA) throw new Error("SSE data exceeded"); this.data.push(value); }
    else if (field === "id" && !value.includes("\0")) this.id = value;
    else if (field === "event") this.event = value || "message";
    else if (field === "retry" && /^\d{1,8}$/.test(value)) this.retry = Math.min(30_000, Math.max(500, Number(value)));
  }

  private dispatch(emit: (message: SSEMessage) => void) {
    if (this.data.length || this.retry !== undefined) emit({ id: this.id, event: this.event, data: this.data.join("\n"), retry: this.retry });
    this.data = []; this.dataBytes = 0; this.event = "message"; this.retry = undefined;
  }
}

export function delay(ms: number, signal: AbortSignal) {
	if (signal.aborted) return Promise.reject(signal.reason);
  return new Promise<void>((resolve, reject) => {
    const done = () => { signal.removeEventListener("abort", aborted); resolve(); };
    const timer = window.setTimeout(done, ms);
    const aborted = () => { clearTimeout(timer); signal.removeEventListener("abort", aborted); reject(signal.reason); };
    signal.addEventListener("abort", aborted, { once: true });
  });
}

function retryAfter(response: Response) {
  const seconds = Number(response.headers.get("Retry-After")); return Number.isFinite(seconds) ? Math.min(30_000, Math.max(500, seconds * 1000)) : 1_000;
}

export async function streamEvents(path: string, onMessage: (message: SSEMessage) => void, signal: AbortSignal) {
  let lastEventID = "", backoff = 500, authRefreshUsed = false;
  while (!signal.aborted) {
	const attempt = new AbortController(); const abortAttempt = () => attempt.abort(signal.reason); signal.addEventListener("abort", abortAttempt, { once: true });
	let headerTimer = window.setTimeout(() => attempt.abort(new DOMException("SSE connect timeout", "TimeoutError")), 5_000);
    let reader: ReadableStreamDefaultReader<Uint8Array> | undefined; let messages = 0;
    try {
      const headers: Record<string, string> = { accept: "text/event-stream" }; if (lastEventID) headers["Last-Event-ID"] = lastEventID;
		const bearer = getManagerToken(); if (bearer) headers.Authorization = `Bearer ${bearer}`;
		const res = await fetch(new URL(path, window.location.origin), { credentials: "same-origin", headers, signal: attempt.signal }); window.clearTimeout(headerTimer); headerTimer = 0;
	  if (res.status === 401) { await res.body?.cancel(); if (!authRefreshUsed) { const refreshed=await refreshAuth(); if (refreshed==="refreshed") { authRefreshUsed=true; continue; } if (refreshed==="unavailable") throw new Error("SSE authentication unavailable"); } throw new Error("SSE authentication expired"); }
      if (res.status === 403) { await res.body?.cancel(); throw new Error("SSE authorization failed"); }
      if (res.status === 429) { const retry = retryAfter(res); await res.body?.cancel(); await delay(retry, signal); continue; }
      if (res.status >= 400 && res.status < 500) { await res.body?.cancel(); throw new Error(`SSE terminal response (${res.status})`); }
		const mediaType = res.headers.get("content-type")?.split(";", 1)[0]?.trim().toLowerCase();
		if (!res.ok || !res.body || mediaType !== "text/event-stream") { await res.body?.cancel(); throw new Error(`SSE transient response (${res.status})`); }
      const parser = new SSEParser(); reader = res.body.getReader();
      for (;;) {
        const { done, value } = await reader.read(); if (done) { parser.finish(onMessage); break; }
		parser.push(value, (message) => { messages++; authRefreshUsed = false; if (message.event === "reset") lastEventID = ""; else if (message.id) lastEventID = message.id; if (message.retry) backoff = message.retry; onMessage(message); });
      }
      if (messages >= 1) backoff = Math.min(backoff, 1_000);
    } catch (error) {
      if (signal.aborted) return;
	  if (error instanceof Error && (error.message.includes("expired") || error.message.includes("authorization") || error.message.includes("terminal"))) throw error;
    } finally {
		window.clearTimeout(headerTimer); signal.removeEventListener("abort", abortAttempt);
      if (reader) { try { await reader.cancel(); } catch { /* connection already closed */ } reader.releaseLock(); }
    }
    const jitter = Math.floor(Math.random() * Math.max(1, backoff / 4));
	try { await delay(backoff + jitter, signal); } catch (error) { if (signal.aborted) return; throw error; }
	backoff = Math.min(30_000, backoff * 2);
  }
}
