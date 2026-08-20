import { createContext, type PropsWithChildren, useContext, useEffect, useRef, useState } from "react";
import { getSessionCSRF, refreshSession, setManagerToken, setManagerTokenRefresher, setSessionCSRF } from "@/api/client";
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

// jwtExpMs는 JWT payload의 exp(초)를 ms로 돌려줍니다. 검증은 서버 몫이고
// 여기서는 갱신 시점 계산에만 씁니다 — 파싱 실패는 undefined입니다.
function jwtExpMs(token: string): number | undefined {
	try {
		const payload = JSON.parse(atob((token.split(".")[1] ?? "").replace(/-/g, "+").replace(/_/g, "/"))) as { exp?: number };
		return typeof payload.exp === "number" ? payload.exp * 1000 : undefined;
	} catch {
		return undefined;
	}
}

export function AuthGate({ children }: PropsWithChildren) {
	const enabled = document.querySelector('meta[name="k8s-auth-session"][content="enabled"]') !== null;
	// manager 모드: Dhub2.0(dhub2-manager)의 로그인 세션(refresh_token 쿠키)에서 access
	// token을 조용히 받아 Bearer로 씁니다. 대시보드는 자체 로그인 화면을 갖지 않습니다.
	const managerOrigin = document.querySelector('meta[name="k8s-auth-manager-origin"]')?.getAttribute("content") ?? "";
	const managerLogin = document.querySelector('meta[name="k8s-auth-manager-login"]')?.getAttribute("content") || managerOrigin;
  const [state, setState] = useState<"loading" | "disabled" | "ready" | "error">(enabled || managerOrigin ? "loading" : "disabled");
  const [session, setSession] = useState<Session>();
	const managerTimer = useRef<number>();

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

	// manager 모드 수명주기: 최초 토큰 획득 → 만료 60초 전 자동 갱신 → 미로그인 상태에서
	// 창 포커스 시 재시도(포털에서 로그인하고 돌아온 경우). 세션 모드와 상호 배타입니다.
	useEffect(() => {
		if (!managerOrigin) return;
		let mounted = true;
		let signedOut = false;
		const schedule = (token: string) => {
			if (managerTimer.current !== undefined) window.clearTimeout(managerTimer.current);
			const exp = jwtExpMs(token);
			const delay = exp === undefined ? 5 * 60_000 : Math.max(10_000, exp - Date.now() - 60_000);
			managerTimer.current = window.setTimeout(() => { void acquire(false); }, Math.min(delay, 2_147_483_647));
		};
		const acquire = async (initial: boolean): Promise<boolean> => {
			try {
				const res = await bounded(undefined, (signal) =>
					fetch(`${managerOrigin}/api/v1/auth/refresh`, { method: "POST", credentials: "include", headers: { accept: "application/json" }, signal }));
				if (res.status === 401) {
					await res.body?.cancel();
					signedOut = true;
					if (mounted) { setManagerToken(""); setSession({ authenticated: false }); setState("ready"); }
					return false;
				}
				if (!res.ok) { await res.body?.cancel(); if (mounted && initial) setState("error"); return false; }
				const body = (await res.json()) as { access_token?: string };
				if (typeof body.access_token !== "string" || !body.access_token) { if (mounted && initial) setState("error"); return false; }
				signedOut = false;
				setManagerToken(body.access_token);
				schedule(body.access_token);
				let displayName = "Dhub2 계정";
				try {
					const ui = await fetch(`${managerOrigin}/userinfo`, { headers: { Authorization: `Bearer ${body.access_token}`, accept: "application/json" } });
					if (ui.ok) { const v = (await ui.json()) as { name?: string; email?: string }; displayName = v.name || v.email || displayName; } else { await ui.body?.cancel(); }
				} catch { /* 표시 이름은 보조 정보 — 실패해도 진행합니다 */ }
				if (mounted) {
					const exp = jwtExpMs(body.access_token) ?? Date.now() + 15 * 60_000;
					setSession({ authenticated: true, principal: { displayName }, capabilities: { canEditDashboard: false, canPublishDashboard: false }, expiresAt: new Date(exp).toISOString(), refreshAt: new Date(Math.max(Date.now() + 1_000, exp - 60_000)).toISOString(), csrfToken: "" } as Session);
					setState("ready");
				}
				return true;
			} catch {
				if (mounted && initial) setState("error");
				return false;
			}
		};
		setManagerTokenRefresher(() => acquire(false));
		void acquire(true);
		const focus = () => { if (signedOut) void acquire(false); };
		window.addEventListener("focus", focus);
		return () => {
			mounted = false;
			window.removeEventListener("focus", focus);
			setManagerTokenRefresher(undefined);
			if (managerTimer.current !== undefined) window.clearTimeout(managerTimer.current);
		};
		// managerOrigin은 문서 메타에서 오는 상수입니다.
		// eslint-disable-next-line react-hooks/exhaustive-deps
	}, [managerOrigin]);

  const logout = async () => {
	if (managerOrigin) {
		// Dhub2.0 세션 자체는 포털이 관리합니다 — 여기서는 이 앱의 토큰만 버립니다.
		if (managerTimer.current !== undefined) window.clearTimeout(managerTimer.current);
		setManagerToken("");
		setSession({ authenticated: false });
		return;
	}
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
	if (managerOrigin) {
		return (
			<main className="auth-state">
				<h1>Sign in required</h1>
				<p>Dhub2 포털에 로그인하면 이 대시보드가 자동으로 연결됩니다. 로그인 후 이 탭으로 돌아오세요.</p>
				<a href={managerLogin} target="_blank" rel="noreferrer">Dhub2 로그인</a>
			</main>
		);
	}
    const returnTo = window.location.pathname + window.location.search;
    return <main className="auth-state"><h1>Sign in required</h1><a href={`/api/v1/auth/login?returnTo=${encodeURIComponent(returnTo)}`}>Sign in</a></main>;
  }
  return <AuthContext.Provider value={{ enabled: state === "ready", session, logout }}>{children}</AuthContext.Provider>;
}
