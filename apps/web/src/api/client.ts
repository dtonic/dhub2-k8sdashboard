import type { ApiError } from "@k8s-dashboard/contracts";
import { readAuthSession } from "./session";

export class HttpError extends Error {
  constructor(readonly status: number, readonly body: ApiError) {
    super(body.message);
    this.name = "HttpError";
  }
}

let csrfToken = "";
export type RefreshResult = "refreshed" | "expired" | "unavailable";
let refreshInFlight: Promise<RefreshResult> | undefined;

// manager 모드(Dhub2.0 인증 위임): AuthGate가 Dhub2.0 세션에서 받아온 access token을
// Bearer로 붙입니다. 갱신 함수도 AuthGate가 등록합니다 — 이 모듈은 보관·부착만 합니다.
let managerToken = "";
let managerRefresh: (() => Promise<boolean>) | undefined;

export function setManagerToken(value: string) {
	managerToken = value;
}

export function setManagerTokenRefresher(fn?: () => Promise<boolean>) {
	managerRefresh = fn;
}

export function getManagerToken() {
	return managerToken;
}

// refreshAuth는 활성 인증 모드(manager Bearer 또는 세션 쿠키)에 맞는 갱신을 수행합니다.
// SSE처럼 두 모드를 구분하고 싶지 않은 호출자용입니다.
export async function refreshAuth(): Promise<RefreshResult> {
	if (managerRefresh) return (await managerRefresh().catch(() => false)) ? "refreshed" : "expired";
	return refreshSession();
}

export function setSessionCSRF(value: string) {
  csrfToken = value;
}

export function getSessionCSRF() {
	return csrfToken;
}

function isUnsafe(method: string) {
  return !["GET", "HEAD", "OPTIONS"].includes(method.toUpperCase());
}

function waitForRefresh(ms: number, signal: AbortSignal) {
	if (signal.aborted) return Promise.reject(signal.reason);
	return new Promise<void>((resolve, reject) => {
		const done = () => { signal.removeEventListener("abort", abort); resolve(); };
		const timer = window.setTimeout(done, ms);
		const abort = () => { window.clearTimeout(timer); signal.removeEventListener("abort", abort); reject(signal.reason); };
		signal.addEventListener("abort", abort, { once: true });
	});
}

export async function refreshSession(notify = true): Promise<RefreshResult> {
  if (!csrfToken) return "expired";
  const csrfBefore = csrfToken;
  refreshInFlight ??= (async (): Promise<RefreshResult> => {
	const controller = new AbortController(); const timeout = window.setTimeout(() => controller.abort(new DOMException("Authentication timeout", "TimeoutError")), 5_000);
	try {
		const res = await fetch("/api/v1/auth/refresh", { method: "POST", credentials: "same-origin", headers: { accept: "application/json", "X-CSRF-Token": csrfToken }, signal: controller.signal });
		if (res.status === 204) { const next=res.headers.get("X-CSRF-Token") ?? ""; if(!/^[A-Za-z0-9_-]{43}$/.test(next)) return "unavailable"; csrfToken=next; return "refreshed"; }
		if (res.status === 401) { await res.body?.cancel(); return "expired"; }
		if (res.status !== 409) { await res.body?.cancel(); return "unavailable"; }
		const retryAfter = Number(res.headers.get("Retry-After") ?? "0"); await res.body?.cancel();
		let wait = Math.min(2_000, Math.max(0, retryAfter * 1_000));
		for (let attempt = 0; attempt < 9; attempt++) {
			await waitForRefresh(wait, controller.signal); wait = 250;
			const session = await fetch("/api/v1/auth/session", { credentials: "same-origin", headers: { accept: "application/json" }, signal: controller.signal });
			if (!session.ok) { await session.body?.cancel(); return session.status === 401 ? "expired" : "unavailable"; }
			const value = await readAuthSession(session, controller.signal);
			if (value.authenticated) { if (value.csrfToken !== csrfBefore) { csrfToken = value.csrfToken; return "refreshed"; } continue; }
			if (!("refreshable" in value)) return "expired";
		}
		return "unavailable";
	} finally { window.clearTimeout(timeout); }
  })().catch(() => "unavailable" as const).finally(() => { refreshInFlight = undefined; });
  const result = await refreshInFlight;
  if (result === "refreshed" && notify) window.dispatchEvent(new Event("dashboard-session-refreshed"));
  if (result === "expired" && notify) {
    csrfToken = "";
    window.dispatchEvent(new Event("dashboard-session-expired"));
  }
  return result;
}

async function request(path: string, init: RequestInit, retry = true): Promise<Response> {
  const method = init.method ?? "GET";
  const headers = new Headers(init.headers);
  headers.set("accept", headers.get("accept") ?? "application/json");
  if (init.body && !headers.has("content-type")) headers.set("content-type", "application/json");
  if (isUnsafe(method) && csrfToken) headers.set("X-CSRF-Token", csrfToken);
  if (managerToken) headers.set("Authorization", `Bearer ${managerToken}`);
  const res = await fetch(new URL(path, window.location.origin), { ...init, headers, credentials: "same-origin" });
	if (res.status === 401 && retry && !path.startsWith("/api/v1/auth/")) {
		if (managerRefresh) {
			const ok = await managerRefresh().catch(() => false);
			if (ok) {
				if (init.signal?.aborted) throw init.signal.reason;
				return request(path, init, false);
			}
			return res;
		}
		const refresh = await refreshSession();
		if (refresh === "refreshed") {
			if (init.signal?.aborted) throw init.signal.reason;
			return request(path, init, false);
		}
		if (refresh === "unavailable") {
			throw new HttpError(503, { code: "auth_unavailable", message: "Authentication temporarily unavailable", requestId: "" });
		}
  }
  return res;
}

async function parseError(res: Response): Promise<never> {
  const body = (await res.json().catch(() => null)) as ApiError | null;
  throw new HttpError(res.status, body ?? {
    code: res.status === 403 ? "forbidden" : "internal",
    message: `Request failed (${res.status})`,
    requestId: res.headers.get("X-Request-ID") ?? "",
  });
}

/**
 * apiGet이 모든 요청에 덧붙이는 mock 시나리오 파라미터.
 *
 * 요청 크기를 미리 재야 하는 쪽(최근 항목 청킹)이 같은 값을 봐야 하므로 여기서만
 * 읽습니다. 두 곳에서 각자 읽으면 한쪽만 바뀌어 계산이 조용히 어긋납니다.
 */
export function currentScenarioParam(): readonly (readonly [string, string])[] {
  const scenario = new URLSearchParams(window.location.search).get("scenario");
  return scenario ? [["scenario", scenario] as const] : [];
}

export async function apiGet<T>(path: string, params: Record<string, string> = {}, signal?: AbortSignal): Promise<T> {
  const url = new URL(path, window.location.origin);
  for (const [key, value] of Object.entries(params)) url.searchParams.set(key, value);
  for (const [key, value] of currentScenarioParam()) url.searchParams.set(key, value);
  const res = await request(url.pathname + url.search, { signal });
  if (!res.ok) return parseError(res);
  return (await res.json()) as T;
}

export async function apiRequest<T>(path: string, init: RequestInit = {}): Promise<T> {
  const res = await request(path, init);
  if (!res.ok) return parseError(res);
  if (res.status === 204) return undefined as T;
  return (await res.json()) as T;
}

export async function apiDownload(path: string, signal?: AbortSignal): Promise<{ blob: Blob; filename: string }> {
  const res = await request(path, { signal });
  if (!res.ok) return parseError(res);
  if (!res.headers.get("content-type")?.startsWith("application/json")) throw new Error("Unexpected export content type");
  const match = /filename="([a-z][a-z0-9-]{0,63}\.json)"/.exec(res.headers.get("content-disposition") ?? "");
  return { blob: await res.blob(), filename: match?.[1] ?? "dashboard.json" };
}
