import { afterEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, render, screen, waitFor } from "@testing-library/react";
import { StrictMode } from "react";
import { AuthGate, useAuth } from "./AuthGate";

function AuthProbe() {
	const auth=useAuth(); const session=auth.session;
	return <><span>{session?.authenticated ? `${session.principal.displayName}:${session.capabilities.canEditDashboard}` : "signed-out"}</span><button onClick={()=>void auth.logout()}>probe logout</button></>;
}

function enableManagerAuth() {
	for (const [name, content] of [
		["k8s-auth-manager-origin", "https://manager.example.test"],
		["k8s-auth-manager-login", "https://portal.example.test/login"],
		["k8s-auth-manager-client-id", "dhub2-portal"],
	]) {
		const meta=document.createElement("meta"); meta.name=name; meta.content=content; document.head.append(meta);
	}
}

function managerAccessToken() {
	return `header.${btoa(JSON.stringify({ exp: Math.floor(Date.now() / 1000) + 900 })).replace(/=/g, "")}.signature`;
}

describe("AuthGate runtime signal", () => {
  afterEach(() => { cleanup(); document.querySelectorAll('meta[name^="k8s-auth-"]').forEach((meta) => meta.remove()); vi.restoreAllMocks(); vi.useRealTimers(); });
  it("adds no bootstrap request when immutable image has no runtime meta", () => {
    const fetchMock=vi.spyOn(globalThis,"fetch"); render(<AuthGate><div>application</div></AuthGate>); expect(screen.getByText("application")).toBeInTheDocument(); expect(fetchMock).not.toHaveBeenCalled();
  });
  it("bootstraps the same image when nginx injects the enabled meta", async () => {
    const meta=document.createElement("meta");meta.name="k8s-auth-session";meta.content="enabled";document.head.append(meta);
	const expiresAt=new Date(Date.now()+60_000).toISOString(),refreshAt=new Date(Date.now()+30_000).toISOString();
    const fetchMock=vi.spyOn(globalThis,"fetch").mockResolvedValue(new Response(JSON.stringify({authenticated:true,principal:{displayName:"operator"},capabilities:{canEditDashboard:true,canPublishDashboard:false},expiresAt,refreshAt,csrfToken:"c".repeat(43)}),{status:200,headers:{"content-type":"application/json"}}));
    render(<AuthGate><div>application</div></AuthGate>); await waitFor(()=>expect(screen.getByText("application")).toBeInTheDocument()); expect(fetchMock).toHaveBeenCalledTimes(1);
  });
	it("fails closed for a malformed runtime meta", () => {
	  const meta=document.createElement("meta");meta.name="k8s-auth-session";meta.content="true";document.head.append(meta);const fetchMock=vi.spyOn(globalThis,"fetch");render(<AuthGate><div>application</div></AuthGate>);expect(screen.getByText("application")).toBeInTheDocument();expect(fetchMock).not.toHaveBeenCalled();
	});
	it("reuses the Portal OIDC refresh cookie before rendering the application", async () => {
		enableManagerAuth(); const token=managerAccessToken();
		const fetchMock=vi.spyOn(globalThis,"fetch")
			.mockResolvedValueOnce(new Response(JSON.stringify({access_token:token}),{status:200,headers:{"content-type":"application/json"}}))
			.mockResolvedValueOnce(new Response(JSON.stringify({name:"Portal Admin"}),{status:200,headers:{"content-type":"application/json"}}));
		render(<AuthGate><AuthProbe/></AuthGate>);
		await waitFor(()=>expect(screen.getByText("Portal Admin:false")).toBeInTheDocument());
		expect(fetchMock).toHaveBeenCalledTimes(2);
		expect(fetchMock.mock.calls[0]?.[0]).toBe("https://manager.example.test/token");
		const init=fetchMock.mock.calls[0]?.[1];
		expect(init?.credentials).toBe("include");
		expect(new Headers(init?.headers).get("content-type")).toBe("application/x-www-form-urlencoded");
		expect(init?.body?.toString()).toBe("grant_type=refresh_token&client_id=dhub2-portal");
	});
	it("converges manager bootstrap after the StrictMode effect cleanup", async () => {
		enableManagerAuth(); const token=managerAccessToken();
		vi.spyOn(globalThis,"fetch").mockImplementation(async (input) => {
			const url=String(input);
			if (url.endsWith("/token")) return new Response(JSON.stringify({access_token:token}),{status:200,headers:{"content-type":"application/json"}});
			if (url.endsWith("/userinfo")) return new Response(JSON.stringify({name:"Strict Admin"}),{status:200,headers:{"content-type":"application/json"}});
			throw new Error(`unexpected request: ${url}`);
		});
		render(<StrictMode><AuthGate><AuthProbe/></AuthGate></StrictMode>);
		await waitFor(()=>expect(screen.getByText("Strict Admin:false")).toBeInTheDocument());
	});
	it("falls back to the legacy refresh endpoint for a Portal local-login cookie", async () => {
		enableManagerAuth(); const token=managerAccessToken();
		const fetchMock=vi.spyOn(globalThis,"fetch")
			.mockResolvedValueOnce(new Response(JSON.stringify({detail:"Invalid refresh token"}),{status:400,headers:{"content-type":"application/json"}}))
			.mockResolvedValueOnce(new Response(JSON.stringify({access_token:token}),{status:200,headers:{"content-type":"application/json"}}))
			.mockResolvedValueOnce(new Response(JSON.stringify({email:"operator@example.test"}),{status:200,headers:{"content-type":"application/json"}}));
		render(<AuthGate><AuthProbe/></AuthGate>);
		await waitFor(()=>expect(screen.getByText("operator@example.test:false")).toBeInTheDocument());
		expect(fetchMock.mock.calls.map(([url])=>url)).toEqual([
			"https://manager.example.test/token",
			"https://manager.example.test/api/v1/auth/refresh",
			"https://manager.example.test/userinfo",
		]);
	});
	it("shows the Portal login page after both refresh contracts reject the cookie", async () => {
		enableManagerAuth();
		const fetchMock=vi.spyOn(globalThis,"fetch")
			.mockResolvedValueOnce(new Response("{}",{status:400}))
			.mockResolvedValueOnce(new Response("{}",{status:401}))
			.mockResolvedValueOnce(new Response("{}",{status:400}));
		render(<AuthGate><div>application</div></AuthGate>);
		await waitFor(()=>expect(screen.getByRole("heading",{name:"D.Hub 계정으로 로그인"})).toBeInTheDocument());
		expect(screen.getByRole("link",{name:"D.Hub 로그인 열기"})).toHaveAttribute("href","https://portal.example.test/login");
		expect(screen.queryByText("application")).not.toBeInTheDocument();
		expect(fetchMock).toHaveBeenCalledTimes(3);
	});
	it.each([
		["404", new Response("not found", { status: 404 })],
		["malformed 200", new Response(JSON.stringify({ authenticated: true, access_token: "leak" }), { status: 200 })],
		["JSONP media type", new Response(JSON.stringify({ authenticated: false }), { status: 200, headers: { "content-type": "application/jsonp" } })],
	])("fails closed for enabled meta with %s", async (_name, response) => {
		const meta=document.createElement("meta");meta.name="k8s-auth-session";meta.content="enabled";document.head.append(meta);
		const fetchMock=vi.spyOn(globalThis,"fetch").mockResolvedValue(response);
		render(<AuthGate><div>application</div></AuthGate>);
		await waitFor(()=>expect(screen.getByRole("heading",{name:"Authentication unavailable"})).toBeInTheDocument());
		expect(screen.queryByText("application")).not.toBeInTheDocument(); expect(fetchMock).toHaveBeenCalledTimes(1);
	});
	it("cancels and unlocks an oversized chunked session body", async () => {
		const meta=document.createElement("meta");meta.name="k8s-auth-session";meta.content="enabled";document.head.append(meta);
		let canceled=false; const stream=new ReadableStream<Uint8Array>({start(controller){controller.enqueue(new Uint8Array(4097));},cancel(){canceled=true;}});
		vi.spyOn(globalThis,"fetch").mockResolvedValue(new Response(stream,{status:200,headers:{"content-type":"application/json"}}));
		render(<AuthGate><div>application</div></AuthGate>); await waitFor(()=>expect(screen.getByRole("heading",{name:"Authentication unavailable"})).toBeInTheDocument());
		expect(canceled).toBe(true); expect(stream.locked).toBe(false);
	});
	it("refreshes once when a retained session has expired authorization claims", async () => {
		const meta=document.createElement("meta");meta.name="k8s-auth-session";meta.content="enabled";document.head.append(meta); const expiresAt=new Date(Date.now()+60_000).toISOString(),refreshAt=new Date(Date.now()+30_000).toISOString();
		const fetchMock=vi.spyOn(globalThis,"fetch")
			.mockResolvedValueOnce(new Response(JSON.stringify({authenticated:false,refreshable:true,csrfToken:"c".repeat(43)}),{status:200,headers:{"content-type":"application/json"}}))
			.mockResolvedValueOnce(new Response(null,{status:204,headers:{"X-CSRF-Token":"n".repeat(43)}}))
			.mockResolvedValueOnce(new Response(JSON.stringify({authenticated:true,principal:{displayName:"operator"},capabilities:{canEditDashboard:true,canPublishDashboard:false},expiresAt,refreshAt,csrfToken:"n".repeat(43)}),{status:200,headers:{"content-type":"application/json"}}));
		render(<AuthGate><div>application</div></AuthGate>); await waitFor(()=>expect(screen.getByText("application")).toBeInTheDocument()); expect(fetchMock).toHaveBeenCalledTimes(3);
	});
	it("accepts a server-authenticated due refresh and schedules it immediately", async () => {
		const meta=document.createElement("meta");meta.name="k8s-auth-session";meta.content="enabled";document.head.append(meta);
		const due={authenticated:true,principal:{displayName:"operator"},capabilities:{canEditDashboard:true,canPublishDashboard:false},expiresAt:new Date(Date.now()+5_000).toISOString(),refreshAt:new Date(Date.now()-2_000).toISOString(),csrfToken:"c".repeat(43)};
		const fresh={...due,expiresAt:new Date(Date.now()+5_000).toISOString(),refreshAt:new Date(Date.now()-1_000).toISOString(),csrfToken:"n".repeat(43)};
		const fetchMock=vi.spyOn(globalThis,"fetch").mockResolvedValueOnce(new Response(JSON.stringify(due),{status:200,headers:{"content-type":"application/json"}})).mockResolvedValueOnce(new Response(null,{status:204,headers:{"X-CSRF-Token":"n".repeat(43)}})).mockResolvedValueOnce(new Response(JSON.stringify(fresh),{status:200,headers:{"content-type":"application/json"}}));
		render(<AuthGate><div>application</div></AuthGate>); await waitFor(()=>expect(fetchMock).toHaveBeenCalledTimes(3),{timeout:2_000}); await waitFor(()=>expect(screen.getByText("application")).toBeInTheDocument()); await new Promise((resolve)=>setTimeout(resolve,250)); expect(fetchMock).toHaveBeenCalledTimes(3);
	});
	it.each([
		["valid unauthenticated", new Response(JSON.stringify({authenticated:false}), {status:200,headers:{"content-type":"application/json"}}), "Sign in required"],
		["provider unavailable", new Response("{}", {status:503}), "Authentication unavailable"],
		["malformed session", new Response("{}", {status:200,headers:{"content-type":"application/json"}}), "Authentication unavailable"],
	])("handles proactive refresh reread: %s", async (_name, reread, heading) => {
		const meta=document.createElement("meta");meta.name="k8s-auth-session";meta.content="enabled";document.head.append(meta);
		const expiresAt=new Date(Date.now()+60_000).toISOString(),refreshAt=new Date(Date.now()+20).toISOString();
		vi.spyOn(globalThis,"fetch")
			.mockResolvedValueOnce(new Response(JSON.stringify({authenticated:true,principal:{displayName:"operator"},capabilities:{canEditDashboard:true,canPublishDashboard:false},expiresAt,refreshAt,csrfToken:"c".repeat(43)}),{status:200,headers:{"content-type":"application/json"}}))
			.mockResolvedValueOnce(new Response(null,{status:204,headers:{"X-CSRF-Token":"n".repeat(43)}}))
			.mockResolvedValueOnce(reread);
		render(<AuthGate><div>application</div></AuthGate>);
		await waitFor(()=>expect(screen.getByRole("heading",{name:heading})).toBeInTheDocument());
	});
	it("bounds a stalled session body and releases it on timeout", async () => {
		vi.useFakeTimers(); const meta=document.createElement("meta");meta.name="k8s-auth-session";meta.content="enabled";document.head.append(meta);
		let canceled=false; const stream=new ReadableStream<Uint8Array>({cancel(){canceled=true;}});
		vi.spyOn(globalThis,"fetch").mockResolvedValue(new Response(stream,{status:200,headers:{"content-type":"application/json"}}));
		render(<AuthGate><div>application</div></AuthGate>); await act(async()=>{await vi.advanceTimersByTimeAsync(5_001);});
		expect(screen.getByRole("heading",{name:"Authentication unavailable"})).toBeInTheDocument(); expect(canceled).toBe(true); expect(stream.locked).toBe(false); expect(vi.getTimerCount()).toBe(0);
	});
	it("atomically rereads refreshed identity and logout uses the latest central CSRF", async () => {
		const meta=document.createElement("meta");meta.name="k8s-auth-session";meta.content="enabled";document.head.append(meta);
		const initial={authenticated:true,principal:{displayName:"Admin"},capabilities:{canEditDashboard:true,canPublishDashboard:true},expiresAt:new Date(Date.now()+120_000).toISOString(),refreshAt:new Date(Date.now()+60_000).toISOString(),csrfToken:"c".repeat(43)};
		const refreshed={authenticated:true,principal:{displayName:"Viewer"},capabilities:{canEditDashboard:false,canPublishDashboard:false},expiresAt:new Date(Date.now()+180_000).toISOString(),refreshAt:new Date(Date.now()+90_000).toISOString(),csrfToken:"n".repeat(43)};
		const fetchMock=vi.spyOn(globalThis,"fetch").mockResolvedValueOnce(new Response(JSON.stringify(initial),{status:200,headers:{"content-type":"application/json"}})).mockResolvedValueOnce(new Response(JSON.stringify(refreshed),{status:200,headers:{"content-type":"application/json"}})).mockResolvedValueOnce(new Response(null,{status:204}));
		render(<AuthGate><AuthProbe/></AuthGate>); await waitFor(()=>expect(screen.getByText("Admin:true")).toBeInTheDocument());
		window.dispatchEvent(new Event("dashboard-session-refreshed")); await waitFor(()=>expect(screen.getByText("Viewer:false")).toBeInTheDocument());
		screen.getByRole("button",{name:"probe logout"}).click(); await waitFor(()=>expect(screen.getByText("Sign in required")).toBeInTheDocument());
		expect(new Headers(fetchMock.mock.calls[2]![1]?.headers).get("X-CSRF-Token")).toBe("n".repeat(43));
	});
	it("converges one stale cross-tab CSRF on logout within the shared deadline", async () => {
		const meta=document.createElement("meta");meta.name="k8s-auth-session";meta.content="enabled";document.head.append(meta);
		const initial={authenticated:true,principal:{displayName:"Admin"},capabilities:{canEditDashboard:true,canPublishDashboard:true},expiresAt:new Date(Date.now()+120_000).toISOString(),refreshAt:new Date(Date.now()+60_000).toISOString(),csrfToken:"c".repeat(43)};
		const latest={...initial,csrfToken:"n".repeat(43)};
		const fetchMock=vi.spyOn(globalThis,"fetch").mockResolvedValueOnce(new Response(JSON.stringify(initial),{status:200,headers:{"content-type":"application/json"}})).mockResolvedValueOnce(new Response("{}",{status:403})).mockResolvedValueOnce(new Response(JSON.stringify(latest),{status:200,headers:{"content-type":"application/json"}})).mockResolvedValueOnce(new Response(null,{status:204}));
		render(<AuthGate><AuthProbe/></AuthGate>); await waitFor(()=>expect(screen.getByText("Admin:true")).toBeInTheDocument()); screen.getByRole("button",{name:"probe logout"}).click(); await waitFor(()=>expect(screen.getByText("Sign in required")).toBeInTheDocument());
		expect(new Headers(fetchMock.mock.calls[1]![1]?.headers).get("X-CSRF-Token")).toBe("c".repeat(43)); expect(new Headers(fetchMock.mock.calls[3]![1]?.headers).get("X-CSRF-Token")).toBe("n".repeat(43));
	});
	it.each([
		["already removed", [new Response("{}",{status:401})]],
		["removed during stale-CSRF convergence", [new Response("{}",{status:403}),new Response(JSON.stringify({authenticated:false}),{status:200,headers:{"content-type":"application/json"}})]],
	])("treats logout as signed out when the session is %s", async (_name,responses) => {
		const meta=document.createElement("meta");meta.name="k8s-auth-session";meta.content="enabled";document.head.append(meta);
		const initial={authenticated:true,principal:{displayName:"Admin"},capabilities:{canEditDashboard:true,canPublishDashboard:true},expiresAt:new Date(Date.now()+120_000).toISOString(),refreshAt:new Date(Date.now()+60_000).toISOString(),csrfToken:"c".repeat(43)};
		const fetchMock=vi.spyOn(globalThis,"fetch").mockResolvedValueOnce(new Response(JSON.stringify(initial),{status:200,headers:{"content-type":"application/json"}})); for(const response of responses)fetchMock.mockResolvedValueOnce(response);
		render(<AuthGate><AuthProbe/></AuthGate>); await waitFor(()=>expect(screen.getByText("Admin:true")).toBeInTheDocument()); screen.getByRole("button",{name:"probe logout"}).click(); await waitFor(()=>expect(screen.getByText("Sign in required")).toBeInTheDocument()); expect(screen.queryByRole("heading",{name:"Authentication unavailable"})).not.toBeInTheDocument();
	});
});
