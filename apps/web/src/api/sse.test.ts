import { afterEach, describe, expect, it, vi } from "vitest";
vi.mock("./client", () => ({ refreshAuth: vi.fn(), getManagerToken: () => "" }));
import { refreshAuth } from "./client";
import { delay, SSEParser, streamEvents, type SSEMessage } from "./sse";

afterEach(() => { vi.restoreAllMocks(); vi.useRealTimers(); });

describe("SSEParser", () => {
	const bytes = (value: string) => new TextEncoder().encode(value);
  it("parses fragmented CRLF, comments and multi-line data", () => {
    const parser = new SSEParser(); const events: SSEMessage[] = [];
	parser.push(bytes(": keepalive\r\nid: 4\r\nevent: pod\r\nda"), (value) => events.push(value));
	parser.push(bytes("ta: one\r\ndata: two\r\nretry: 1200\r\n\r\n"), (value) => events.push(value));
    expect(events).toEqual([{ id: "4", event: "pod", data: "one\ntwo", retry: 1200 }]);
  });

  it("bounds line and data buffers", () => {
	expect(() => new SSEParser().push(bytes(`data: ${"x".repeat(70_000)}\n`), () => undefined)).toThrow();
	expect(() => new SSEParser().push(bytes("x".repeat(140_000)), () => undefined)).toThrow();
	});

	it("tracks UTF-8 wire bytes across fragmented code points", () => {
	  const parser = new SSEParser(); const events: SSEMessage[] = []; const wire = bytes("data: 한글\n\n");
	  parser.push(wire.slice(0, 7), (value) => events.push(value)); parser.push(wire.slice(7), (value) => events.push(value));
	  expect(events[0]?.data).toBe("한글");
	});
});

describe("SSE reconnect delay", () => {
	it("leaves no timer for a pre-aborted or newly aborted signal", async () => {
		vi.useFakeTimers();
		const before = new AbortController(); before.abort(new Error("stopped"));
		await expect(delay(30_000, before.signal)).rejects.toThrow("stopped");
		expect(vi.getTimerCount()).toBe(0);
		const during = new AbortController(); const pending = delay(30_000, during.signal); during.abort(new Error("stopped"));
		await expect(pending).rejects.toThrow("stopped"); expect(vi.getTimerCount()).toBe(0);
		vi.useRealTimers();
	});
});

describe("SSE transport lifecycle", () => {
	it("times out stalled headers, retries, and exits cleanly on external abort", async () => {
		vi.useFakeTimers(); vi.spyOn(Math,"random").mockReturnValue(0);
		const fetchMock=vi.spyOn(globalThis,"fetch").mockImplementation((_input,init)=>new Promise((_resolve,reject)=>init?.signal?.addEventListener("abort",()=>reject(init.signal?.reason),{once:true})));
		const controller=new AbortController(); const running=streamEvents("/events/stream",()=>undefined,controller.signal);
		await vi.advanceTimersByTimeAsync(5_001); await vi.advanceTimersByTimeAsync(501); expect(fetchMock.mock.calls.length).toBeGreaterThanOrEqual(2);
		controller.abort(new Error("stopped")); await expect(running).resolves.toBeUndefined(); expect(vi.getTimerCount()).toBe(0);
	});

	it.each([401,403,418])("cancels terminal %s response bodies", async (status) => {
		vi.mocked(refreshAuth).mockResolvedValue("expired"); let canceled=false;
		const body=new ReadableStream<Uint8Array>({cancel(){canceled=true;}}); vi.spyOn(globalThis,"fetch").mockResolvedValue(new Response(body,{status}));
		await expect(streamEvents("/events/stream",()=>undefined,new AbortController().signal)).rejects.toThrow(); expect(canceled).toBe(true); expect(body.locked).toBe(false);
	});

	it.each([
		[429,"text/plain"],
		[503,"text/plain"],
		[200,"text/event-streamx"],
	] as const)("cancels reconnectable/non-stream status %s body", async (status,contentType) => {
		let canceled=false; const controller=new AbortController(); const body=new ReadableStream<Uint8Array>({cancel(){canceled=true;}});
		vi.spyOn(globalThis,"fetch").mockResolvedValue(new Response(body,{status,headers:{"content-type":contentType,"Retry-After":"30"}}));
		const running=streamEvents("/events/stream",()=>undefined,controller.signal); await vi.waitFor(()=>expect(canceled).toBe(true)); controller.abort(new Error("stopped")); await expect(running).resolves.toBeUndefined(); expect(body.locked).toBe(false);
	});

	it("clears the header timeout while an established stream remains open", async () => {
		vi.useFakeTimers(); let streamController:ReadableStreamDefaultController<Uint8Array>|undefined; let canceled=false;
		const body=new ReadableStream<Uint8Array>({start(controller){streamController=controller;},cancel(){canceled=true;}});
		const fetchMock=vi.spyOn(globalThis,"fetch").mockImplementation(async (_input,init)=>{init?.signal?.addEventListener("abort",()=>streamController?.error(init.signal?.reason),{once:true});return new Response(body,{status:200,headers:{"content-type":"text/event-stream; charset=utf-8"}});});
		const controller=new AbortController(); const running=streamEvents("/events/stream",()=>undefined,controller.signal); await vi.advanceTimersByTimeAsync(6_000); expect(fetchMock).toHaveBeenCalledTimes(1);
		controller.abort(new Error("stopped")); await expect(running).resolves.toBeUndefined(); expect(canceled || !body.locked).toBe(true); expect(body.locked).toBe(false); expect(vi.getTimerCount()).toBe(0);
	});
});
