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
  const res = await fetch(new URL(path, window.location.origin), { ...init, headers, credentials: "same-origin" });
	if (res.status === 401 && retry && !path.startsWith("/api/v1/auth/")) {
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

export async function apiGet<T>(path: string, params: Record<string, string> = {}, signal?: AbortSignal): Promise<T> {
  const url = new URL(path, window.location.origin);
  for (const [key, value] of Object.entries(params)) url.searchParams.set(key, value);
  const scenario = new URLSearchParams(window.location.search).get("scenario");
  if (scenario) url.searchParams.set("scenario", scenario);
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
