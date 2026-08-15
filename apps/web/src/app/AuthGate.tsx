import { createContext, type PropsWithChildren, useContext, useEffect, useState } from "react";
import { getSessionCSRF, refreshSession, setSessionCSRF } from "@/api/client";
import { readAuthSession, type AuthSession as Session } from "@/api/session";

type AuthState = { enabled: boolean; session?: Session; logout: () => Promise<void> };
const AuthContext = createContext<AuthState>({ enabled: false, logout: async () => undefined });
export const useAuth = () => useContext(AuthContext);

async function bounded<T>(parent: AbortSignal | undefined, work: (signal: AbortSignal) => Promise<T>): Promise<T> {
	const controller = new AbortController(); const abort = () => controller.abort(parent?.reason); parent?.addEventListener("abort", abort, { once: true });
	if (parent?.aborted) controller.abort(parent.reason);
	const timer = window.setTimeout(() => controller.abort(new DOMException("Authentication timeout", "TimeoutError")), 5_000);
	try { return await work(controller.signal); } finally { window.clearTimeout(timer); parent?.removeEventListener("abort", abort); }
}

const fetchSession = (parent?: AbortSignal) => bounded(parent, async (signal) => {
	const response = await fetch("/api/v1/auth/session", { credentials: "same-origin", headers: { accept: "application/json" }, signal });
	if (!response.ok) throw new Error("session unavailable");
	return readAuthSession(response, signal);
});

export function AuthGate({ children }: PropsWithChildren) {
	const enabled = document.querySelector('meta[name="k8s-auth-session"][content="enabled"]') !== null;
  const [state, setState] = useState<"loading" | "disabled" | "ready" | "error">(enabled ? "loading" : "disabled");
  const [session, setSession] = useState<Session>();

  useEffect(() => {
    if (!enabled) return;
	let mounted=true; const controller = new AbortController();
		fetchSession(controller.signal).then(async (value) => {
		if(!mounted)return;
		if (!value.authenticated && "refreshable" in value) {
			setSessionCSRF(value.csrfToken); const refreshed=await refreshSession(false);
			if(!mounted)return;
			if(refreshed==="expired"){setSessionCSRF("");setSession({authenticated:false});setState("ready");return;}
			if(refreshed!=="refreshed")throw new Error("session refresh failed");
			const latest=await fetchSession(controller.signal); if(!mounted)return; setSessionCSRF(latest.authenticated?latest.csrfToken:"");setSession(latest);setState("ready");
			return;
		}
		if(!mounted)return;
		setSessionCSRF(value.authenticated ? value.csrfToken : "");
        setSession(value); setState("ready");
      }).catch((error: unknown) => { if (mounted && (error as Error).name !== "AbortError") setState("error"); });
    return () => {mounted=false;controller.abort();};
  }, [enabled]);

	useEffect(() => {
	  if (!enabled || !session?.authenticated || !session.refreshAt) return;
	  let mounted = true; const controller = new AbortController();
	  const untilRefresh = new Date(session.refreshAt).getTime() - Date.now();
	  // A due server timestamp is valid (browser clocks and network latency vary),
	  // but retain a small floor so repeated short-lived tokens cannot hot-loop.
	  const delay = untilRefresh <= 0 ? 1_000 : untilRefresh;
	  const timer = window.setTimeout(async () => {
		try {
	    const refreshed = await refreshSession(); if (!mounted) return; if (refreshed === "expired") { setSessionCSRF(""); setSession({ authenticated: false }); setState("ready"); return; } if (refreshed === "unavailable") { setState("error"); return; }
		} catch { if (mounted) setState("error"); }
	  }, Math.min(delay, 2_147_483_647));
	  return () => { mounted = false; controller.abort(); window.clearTimeout(timer); };
	}, [enabled, session?.authenticated, session?.authenticated ? session.refreshAt : undefined]);

  useEffect(() => {
    const expired = () => { setSessionCSRF(""); setSession({ authenticated: false }); setState("ready"); };
    window.addEventListener("dashboard-session-expired", expired);
    return () => window.removeEventListener("dashboard-session-expired", expired);
  }, []);

	useEffect(() => {
		if (!enabled) return;
		let mounted=true, timer:number|undefined; const controller=new AbortController();
		const refreshed=()=>{ if(timer!==undefined)return; timer=window.setTimeout(async()=>{ timer=undefined; try { const value=await fetchSession(controller.signal); if(!mounted)return; setSessionCSRF(value.authenticated ? value.csrfToken : ""); setSession(value); setState("ready"); } catch { if(mounted)setState("error"); } },0); };
		window.addEventListener("dashboard-session-refreshed",refreshed);
		return()=>{mounted=false;controller.abort();if(timer!==undefined)window.clearTimeout(timer);window.removeEventListener("dashboard-session-refreshed",refreshed);};
	},[enabled]);

  const logout = async () => {
	const outcome = await bounded(undefined, async (signal): Promise<Response | "signed-out"> => {
		const send=()=>fetch("/api/v1/auth/logout", { method: "POST", credentials: "same-origin", headers: { accept: "application/json", "X-CSRF-Token": getSessionCSRF() }, signal });
		let result=await send(); if(result.status===401){await result.body?.cancel();return "signed-out";} if(result.status!==403)return result; await result.body?.cancel();
		const previous=getSessionCSRF(), latest=await fetchSession(signal); if(!latest.authenticated)return "signed-out"; if(latest.csrfToken===previous)return result; setSessionCSRF(latest.csrfToken); setSession(latest); result=await send(); return result;
	}).catch(() => undefined);
	if (outcome === "signed-out") { setSessionCSRF(""); setSession({ authenticated: false }); setState("ready"); return; }
    if (!outcome) { setState("error"); return; }
    if (!outcome.ok) { setState("error"); return; }
    setSessionCSRF(""); setSession({ authenticated: false });
  };

  if (state === "loading") return <main className="auth-state" aria-busy="true">Checking session…</main>;
  if (state === "error") return <main className="auth-state"><h1>Authentication unavailable</h1><button onClick={() => window.location.reload()}>Retry</button></main>;
  if (state === "ready" && !session?.authenticated) {
    const returnTo = window.location.pathname + window.location.search;
    return <main className="auth-state"><h1>Sign in required</h1><a href={`/api/v1/auth/login?returnTo=${encodeURIComponent(returnTo)}`}>Sign in</a></main>;
  }
  return <AuthContext.Provider value={{ enabled: state === "ready", session, logout }}>{children}</AuthContext.Provider>;
}
