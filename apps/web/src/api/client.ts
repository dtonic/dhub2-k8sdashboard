import type { ApiError } from "@k8s-dashboard/contracts";

export class HttpError extends Error {
  constructor(
    readonly status: number,
    readonly body: ApiError,
  ) {
    super(body.message);
    this.name = "HttpError";
  }
}

/**
 * Observability API 호출 래퍼.
 *
 * - 브라우저는 GreptimeDB/Quickwit/Kubernetes API를 직접 호출하지 않습니다. (README §10)
 *   이 함수가 유일한 데이터 진입점입니다.
 * - `signal`을 그대로 전달해 TanStack Query의 요청 취소가 네트워크까지 내려가게 합니다. (README §11)
 * - mock 시나리오(`?scenario=`)는 URL에서 읽어 그대로 전달합니다. 실제 API에서는 무시됩니다.
 */
export async function apiGet<T>(path: string, params: Record<string, string> = {}, signal?: AbortSignal): Promise<T> {
  const url = new URL(path, window.location.origin);
  for (const [k, v] of Object.entries(params)) url.searchParams.set(k, v);

  const scenario = new URLSearchParams(window.location.search).get("scenario");
  if (scenario) url.searchParams.set("scenario", scenario);

  const res = await fetch(url, { signal, headers: { accept: "application/json" } });
  if (!res.ok) {
    const body = (await res.json().catch(() => null)) as ApiError | null;
    throw new HttpError(
      res.status,
      body ?? {
        code: res.status === 403 ? "forbidden" : "internal",
        message: `요청이 실패했습니다 (${res.status})`,
        // 본문이 JSON이 아닐 때의 합성 에러입니다. 상관관계 ID는 응답 헤더에서 가져옵니다.
        requestId: res.headers.get("X-Request-ID") ?? "",
      },
    );
  }
  return (await res.json()) as T;
}

export async function apiRequest<T>(path: string, init: RequestInit = {}): Promise<T> {
  const url = new URL(path, window.location.origin);
  const res = await fetch(url, { ...init, headers: { accept: "application/json", ...(init.body ? { "content-type": "application/json" } : {}), ...init.headers } });
  if (!res.ok) {
    const body = (await res.json().catch(() => null)) as ApiError | null;
    throw new HttpError(res.status, body ?? { code: "internal", message: `Request failed (${res.status})`, requestId: res.headers.get("X-Request-ID") ?? "" });
  }
  if (res.status === 204) return undefined as T;
  return (await res.json()) as T;
}

export async function apiDownload(path: string, signal?: AbortSignal): Promise<{blob:Blob;filename:string}> {
  const res=await fetch(new URL(path,window.location.origin),{signal,headers:{accept:"application/json"}});
  if(!res.ok){const body=(await res.json().catch(()=>null)) as ApiError|null;throw new HttpError(res.status,body??{code:"internal",message:`Request failed (${res.status})`,requestId:res.headers.get("X-Request-ID")??""})}
  if(!res.headers.get("content-type")?.startsWith("application/json"))throw new Error("Unexpected export content type");
  const match=/filename="([a-z][a-z0-9-]{0,63}\.json)"/.exec(res.headers.get("content-disposition")??"");
  return {blob:await res.blob(),filename:match?.[1]??"dashboard.json"};
}
