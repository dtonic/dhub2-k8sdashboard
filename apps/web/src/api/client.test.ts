import { afterEach, describe, expect, it, vi } from "vitest";
import { apiGet, HttpError, refreshSession, setSessionCSRF } from "./client";

afterEach(() => { vi.restoreAllMocks(); vi.useRealTimers(); });

describe("apiGet", () => {
  it("forwards cancellation and uses only the BFF path", async () => {
    const controller = new AbortController();
    const fetchMock = vi.spyOn(globalThis, "fetch").mockResolvedValue(
      new Response(JSON.stringify({ ok: true }), { status: 200, headers: { "content-type": "application/json" } }),
    );

    await expect(apiGet("/api/v1/scope", {}, controller.signal)).resolves.toEqual({ ok: true });
    const [url, init] = fetchMock.mock.calls[0]!;
    expect(new URL(String(url)).pathname).toBe("/api/v1/scope");
    expect(init?.signal).toBe(controller.signal);
    expect(new Headers(init?.headers).get("accept")).toBe("application/json");
    expect(init?.credentials).toBe("same-origin");
  });

  it("preserves a structured forbidden response", async () => {
    vi.spyOn(globalThis, "fetch").mockResolvedValue(
      new Response(JSON.stringify({ code: "forbidden", message: "denied", requestId: "req-1" }), {
        status: 403,
        headers: { "content-type": "application/json" },
      }),
    );

    const error = await apiGet("/api/v1/scope").catch((cause: unknown) => cause);
    expect(error).toBeInstanceOf(HttpError);
    expect((error as HttpError).status).toBe(403);
    expect((error as HttpError).body.code).toBe("forbidden");
  });

	it("surfaces refresh unavailability instead of the original authorization 401", async () => {
		setSessionCSRF("c".repeat(43));
		const fetchMock = vi.spyOn(globalThis, "fetch")
			.mockResolvedValueOnce(new Response("{}", { status: 401, headers: { "content-type": "application/json" } }))
			.mockResolvedValueOnce(new Response("{}", { status: 503, headers: { "content-type": "application/json" } }));
		const error = await apiGet("/api/v1/scope").catch((cause: unknown) => cause);
		expect(error).toBeInstanceOf(HttpError);
		expect((error as HttpError).status).toBe(503);
		expect((error as HttpError).body.code).toBe("auth_unavailable");
		expect(fetchMock).toHaveBeenCalledTimes(2);
	});
});

describe("refreshSession", () => {
	it("converges a cross-tab 409 through one bounded session read", async () => {
		setSessionCSRF("c".repeat(43));
		const expiresAt=new Date(Date.now()+60_000).toISOString(),refreshAt=new Date(Date.now()+30_000).toISOString();
		const fetchMock = vi.spyOn(globalThis, "fetch")
			.mockResolvedValueOnce(new Response(JSON.stringify({ code: "refresh_conflict", message: "busy", requestId: "r" }), { status: 409, headers: { "Retry-After": "0" } }))
			.mockResolvedValueOnce(new Response(JSON.stringify({ authenticated: true, principal:{displayName:"operator"},capabilities:{canEditDashboard:true,canPublishDashboard:false},expiresAt,refreshAt,csrfToken: "n".repeat(43) }), { status: 200, headers: { "content-type": "application/json" } }));
		await expect(refreshSession()).resolves.toBe("refreshed");
		expect(fetchMock).toHaveBeenCalledTimes(2);
		expect(String(fetchMock.mock.calls[1]![0])).toBe("/api/v1/auth/session");
	});
	it("keeps refreshable state while a slower cross-tab winner commits", async () => {
		vi.useFakeTimers(); setSessionCSRF("c".repeat(43));
		const expiresAt=new Date(Date.now()+60_000).toISOString(),refreshAt=new Date(Date.now()+30_000).toISOString();
		const fetchMock = vi.spyOn(globalThis, "fetch")
			.mockResolvedValueOnce(new Response("{}", { status: 409, headers: { "Retry-After": "1" } }))
			.mockResolvedValueOnce(new Response(JSON.stringify({ authenticated:true,principal:{displayName:"operator"},capabilities:{canEditDashboard:true,canPublishDashboard:false},expiresAt,refreshAt,csrfToken:"c".repeat(43) }), { status:200,headers:{"content-type":"application/json"} }))
			.mockResolvedValueOnce(new Response(JSON.stringify({ authenticated:true,principal:{displayName:"operator"},capabilities:{canEditDashboard:true,canPublishDashboard:false},expiresAt,refreshAt,csrfToken:"n".repeat(43) }), { status:200,headers:{"content-type":"application/json"} }));
		const result=refreshSession(); await vi.advanceTimersByTimeAsync(1_000); await vi.advanceTimersByTimeAsync(250);
		await expect(result).resolves.toBe("refreshed"); expect(fetchMock).toHaveBeenCalledTimes(3); expect(vi.getTimerCount()).toBe(0);
	});
	it("keeps session state across a transient provider outage", async () => {
		setSessionCSRF("c".repeat(43)); const expired=vi.fn(), refreshed=vi.fn(); window.addEventListener("dashboard-session-expired",expired); window.addEventListener("dashboard-session-refreshed",refreshed);
		const fetchMock=vi.spyOn(globalThis,"fetch").mockResolvedValueOnce(new Response("{}",{status:503})).mockResolvedValueOnce(new Response(null,{status:204,headers:{"X-CSRF-Token":"n".repeat(43)}}));
		await expect(refreshSession()).resolves.toBe("unavailable"); await expect(refreshSession()).resolves.toBe("refreshed");
		expect(expired).not.toHaveBeenCalled(); expect(refreshed).toHaveBeenCalledTimes(1); expect(refreshed.mock.calls[0]![0]).not.toBeInstanceOf(CustomEvent); expect(new Headers(fetchMock.mock.calls[1]![1]?.headers).get("X-CSRF-Token")).toBe("c".repeat(43)); window.removeEventListener("dashboard-session-expired",expired); window.removeEventListener("dashboard-session-refreshed",refreshed);
	});
	it("bounds all 409 convergence header waits to one deadline and clears singleflight", async () => {
		vi.useFakeTimers(); setSessionCSRF("c".repeat(43));
		const fetchMock=vi.spyOn(globalThis,"fetch")
			.mockResolvedValueOnce(new Response("{}",{status:409,headers:{"Retry-After":"0"}}))
			.mockImplementationOnce((_input,init)=>new Promise((_resolve,reject)=>init?.signal?.addEventListener("abort",()=>reject(init.signal?.reason),{once:true})))
			.mockResolvedValueOnce(new Response(null,{status:204,headers:{"X-CSRF-Token":"n".repeat(43)}}));
		const first=refreshSession(); await vi.advanceTimersByTimeAsync(5_001); await expect(first).resolves.toBe("unavailable"); expect(vi.getTimerCount()).toBe(0);
		await expect(refreshSession()).resolves.toBe("refreshed"); expect(fetchMock).toHaveBeenCalledTimes(3);
	});
	it("cancels and unlocks a stalled 409 session body at the overall deadline", async () => {
		vi.useFakeTimers(); setSessionCSRF("c".repeat(43)); let canceled=false;
		const body=new ReadableStream<Uint8Array>({cancel(){canceled=true;}});
		vi.spyOn(globalThis,"fetch").mockResolvedValueOnce(new Response("{}",{status:409,headers:{"Retry-After":"0"}})).mockResolvedValueOnce(new Response(body,{status:200,headers:{"content-type":"application/json"}}));
		const result=refreshSession(); await vi.advanceTimersByTimeAsync(5_001); await expect(result).resolves.toBe("unavailable"); expect(canceled).toBe(true); expect(body.locked).toBe(false); expect(vi.getTimerCount()).toBe(0);
	});
});
