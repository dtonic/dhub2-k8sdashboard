import type { ResourceDetailResponse, ResourceDryRunResponse } from "@k8s-dashboard/contracts";
import { apiRequest, HttpError } from "@/api/client";

/**
 * 변경 검토 dry-run 전송 계층 (ADR 0019 Phase 1)
 * --------------------------------------------------------------------------
 * 이 모듈은 **명령형**입니다. TanStack의 query/mutation을 쓰지 않습니다 —
 * 그 캐시에 넣는 순간 raw 매니페스트가 요청이 끝난 뒤에도 메모리에 남고,
 * devtools·재시도·재수화 경로로 새어 나갈 자리가 생깁니다.
 *
 * 매니페스트가 존재하는 곳은 두 군데뿐입니다: 편집기의 컴포넌트 상태와 POST 본문.
 * 여기서 만드는 오류 문자열에도, 경로에도, 응답에도 매니페스트는 실리지 않습니다.
 * localStorage·sessionStorage·URL·console 어디에도 쓰지 않습니다.
 */

/** 계약의 절대 상한입니다. 서버가 더 낮게 설정할 수 있고, 최종 판정은 서버가 합니다. */
export const DRY_RUN_ABSOLUTE_MAX_BYTES = 1 << 20;

/** 배포 기본 상한입니다. **클라이언트 거절 기준이 아니라 안내 문구용**입니다. */
export const DRY_RUN_DEPLOY_DEFAULT_BYTES = 256 << 10;

const encoder = typeof TextEncoder !== "undefined" ? new TextEncoder() : undefined;

/** UTF-8 바이트 길이입니다. 문자 수가 아니라 바이트가 서버 상한의 단위입니다. */
export function manifestByteLength(text: string): number {
  if (encoder) return encoder.encode(text).length;
  return new Blob([text]).size;
}

export type LocalReject = "empty" | "too_large";

/**
 * 보내기 전에 확실히 거절되는 것만 막습니다.
 *
 * 정확히 1MiB는 통과시킵니다 — 계약의 상한은 "초과"이고, 경계에서 클라이언트가
 * 서버보다 엄격하면 서버가 허용하는 입력을 UI가 막게 됩니다.
 */
export function localReject(text: string): LocalReject | undefined {
  if (text.trim() === "") return "empty";
  if (manifestByteLength(text) > DRY_RUN_ABSOLUTE_MAX_BYTES) return "too_large";
  return undefined;
}

export const LOCAL_REJECT_MESSAGES: Record<LocalReject, string> = {
  empty: "검토할 매니페스트가 비어 있습니다.",
  too_large:
    "매니페스트가 1MiB를 넘습니다. 배포 기본 상한은 256KiB이며 서버가 최종 판정합니다(413).",
};

/**
 * 요청 하나의 대상을 식별하는 키입니다.
 *
 * uid만으로는 부족합니다 — 같은 객체라도 resourceVersion이 바뀌면 검토 기준이
 * 달라지고, 클러스터·GVR·namespace가 다르면 아예 다른 대상입니다. 늦게 도착한
 * 응답이 새 대상의 화면을 덮지 않게 하려면 이 전부를 비교해야 합니다.
 *
 * 구분자를 골라 잇지 않고 **JSON 배열로 직렬화**합니다. 어떤 구분자를 쓰든 그
 * 문자가 값에 들어올 수 있으면 ("a|b","c")와 ("a","b|c")가 같은 키가 됩니다.
 * JSON은 값 안의 따옴표·역슬래시를 이스케이프하므로 배열 → 문자열이 단사입니다.
 */
export function identityKey(detail: ResourceDetailResponse): string {
  return JSON.stringify([
    detail.clusterId,
    detail.group,
    detail.version,
    detail.resource,
    detail.namespace ?? "",
    detail.name,
    detail.uid,
    detail.resourceVersion,
  ]);
}

/** 정본 경로입니다. 네 세그먼트를 모두 인코딩합니다. */
export function dryRunPath(
  detail: Pick<ResourceDetailResponse, "clusterId" | "group" | "version" | "resource">,
): string {
  const cluster = encodeURIComponent(detail.clusterId);
  const group = encodeURIComponent(detail.group);
  const version = encodeURIComponent(detail.version);
  const resource = encodeURIComponent(detail.resource);
  return `/api/v1/clusters/${cluster}/resources/${group}/${version}/${resource}/object/dry-run`;
}

/**
 * 검토 요청 하나입니다.
 *
 * 본문에 담는 것은 **상세가 서버에서 받아 온 신원**과 매니페스트뿐입니다.
 * 화면이 지어낸 값도, 추가 옵션도 없습니다 — force·token·동사 같은 필드는
 * 계약에 없고 여기서도 만들지 않습니다.
 */
export function submitDryRun(args: {
  detail: ResourceDetailResponse;
  manifest: string;
  signal: AbortSignal;
}): Promise<ResourceDryRunResponse> {
  const { detail, manifest, signal } = args;
  const body = {
    apiVersion: detail.apiVersion,
    kind: detail.kind,
    // namespace는 namespaced 리소스에서만 보냅니다. cluster 범위에 붙이면 400입니다.
    ...(detail.namespace ? { namespace: detail.namespace } : {}),
    name: detail.name,
    uid: detail.uid,
    resourceVersion: detail.resourceVersion,
    manifest,
  };
  return apiRequest<ResourceDryRunResponse>(dryRunPath(detail), {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(body),
    signal,
  });
}

/**
 * 취소인지 판별합니다.
 *
 * 두 가지를 함께 봅니다 — 우리가 끊었다는 사실(signal)과 오류의 이름. 401 재시도
 * 경로가 signal.reason을 그대로 던질 수 있어서 이름만 보면 놓칩니다.
 * 취소는 사용자가 만든 오류가 아니므로 화면에 띄우지 않습니다.
 */
export function isAbortError(error: unknown, signal?: AbortSignal): boolean {
  if (signal?.aborted) return true;
  if (typeof DOMException !== "undefined" && error instanceof DOMException) {
    return error.name === "AbortError";
  }
  return typeof error === "object" && error !== null && (error as { name?: unknown }).name === "AbortError";
}

/**
 * 서버가 돌려준 code만 고정 한국어로 옮깁니다.
 *
 * **`HttpError.body.message`를 쓰지 않습니다.** 서버가 지금은 고정 문장을 보내더라도,
 * 그 값을 그대로 DOM에 넣는 경로를 만들어 두면 어느 시점의 어떤 upstream 문자열이
 * 화면에 뜰지 UI가 보장할 수 없게 됩니다. 알 수 없는 오류는 객체든 문자열이든
 * 절대 표시하지 않고 고정 문구로 대체합니다.
 */
const REVIEW_MESSAGES: Record<string, string> = {
  /* 요청이 성립하지 않음 */
  bad_request: "요청 형식이 올바르지 않습니다. 화면을 새로고침한 뒤 다시 시도하세요.",
  unsupported_media_type: "요청 형식이 올바르지 않습니다. 화면을 새로고침한 뒤 다시 시도하세요.",
  invalid_filter: "요청 값이 서버 상한을 벗어났습니다.",

  /* 매니페스트 자체 */
  invalid_manifest:
    "매니페스트를 해석하지 못했습니다. 문서 하나여야 하고 중복 키·anchor·alias는 쓸 수 없습니다.",
  manifest_mismatch: "매니페스트가 가리키는 대상이 지금 열어 둔 객체와 다릅니다.",
  manifest_too_large: "매니페스트가 서버 상한을 넘었습니다. 내용을 줄여 다시 시도하세요.",

  /* 신원·동시성 */
  resource_version_mismatch: "객체가 그 사이에 바뀌었습니다. 목록을 새로고침한 뒤 다시 검토하세요.",
  uid_mismatch: "같은 이름의 다른 객체로 교체되었습니다. 목록을 새로고침하세요.",
  not_found: "대상을 찾을 수 없습니다. 목록을 새로고침하세요.",

  /* 정책·권한 */
  dryrun_resource_denied: "이 리소스는 변경 검토 대상이 아닙니다.",
  namespace_access_denied: "이 Namespace에 대한 권한이 없습니다.",
  cluster_scope_required: "클러스터 범위 리소스는 클러스터 전체 권한이 필요합니다.",
  forbidden: "변경 검토 권한이 없습니다.",
  resource_not_allowlisted: "이 리소스는 탐색·검토 대상으로 등록되어 있지 않습니다.",

  /* 배포 상태 */
  dryrun_unavailable: "이 배포에서는 변경 검토를 사용할 수 없습니다.",
  resources_unavailable: "이 배포에서는 리소스 탐색을 사용할 수 없습니다.",
  resource_syncing: "리소스 캐시를 동기화하는 중입니다. 잠시 후 다시 시도하세요.",
  resource_not_served: "클러스터가 이 API를 제공하지 않습니다.",

  /* upstream */
  dryrun_forbidden: "서버에 이 리소스의 검토 권한이 없습니다. 관리자에게 문의하세요.",
  dryrun_rate_limited: "검토 요청 한도를 초과했습니다. 잠시 후 다시 시도하세요.",
  object_too_large: "검토 결과가 응답 한도를 넘어 표시할 수 없습니다.",
  upstream_unavailable: "클러스터가 검토를 끝내지 못했습니다.",
  upstream_timeout: "클러스터 응답 시간이 초과되었습니다.",
};

/** 알 수 없는 실패에 쓰는 유일한 문구입니다. */
export const REVIEW_FALLBACK_MESSAGE = "검토 요청을 처리하지 못했습니다. 잠시 후 다시 시도하세요.";

/** 응답이 요청한 대상과 다를 때의 문구입니다. 응답 내용은 표시하지 않습니다. */
export const REVIEW_IDENTITY_MISMATCH_MESSAGE =
  "검토 결과가 요청한 대상과 일치하지 않아 표시하지 않습니다. 다시 시도하세요.";

/**
 * 표에서 **고정 문구만** 꺼냅니다.
 *
 * 평범한 객체 조회는 프로토타입 키에 뚫려 있습니다 — `table["__proto__"]`는
 * Object.prototype을, `table["constructor"]`·`table["toString"]`은 함수를 돌려주고
 * 그 값이 truthy라서 그대로 렌더 경로로 흘러갑니다. React는 객체를 자식으로 받으면
 * 던지므로, 서버가 보낸 문자열 하나가 화면 전체를 무너뜨릴 수 있습니다.
 *
 * own property이면서 값이 문자열일 때만 known으로 봅니다. 그 외에는 undefined이고
 * 호출자가 고정 문구로 대체합니다.
 */
export function fixedTextFor(table: Record<string, string>, key: string): string | undefined {
  if (!Object.prototype.hasOwnProperty.call(table, key)) return undefined;
  const value = table[key];
  return typeof value === "string" ? value : undefined;
}

export function reviewErrorMessage(error: unknown): string {
  if (error instanceof HttpError) {
    const known = fixedTextFor(REVIEW_MESSAGES, error.body.code);
    if (known) return known;
  }
  return REVIEW_FALLBACK_MESSAGE;
}

/** 응답이 우리가 물어본 그 대상인지 확인합니다. 하나라도 다르면 렌더하지 않습니다. */
export function responseMatchesDetail(
  response: ResourceDryRunResponse,
  detail: ResourceDetailResponse,
): boolean {
  return (
    response.clusterId === detail.clusterId &&
    response.group === detail.group &&
    response.version === detail.version &&
    response.resource === detail.resource &&
    response.apiVersion === detail.apiVersion &&
    response.kind === detail.kind &&
    (response.namespace ?? "") === (detail.namespace ?? "") &&
    response.name === detail.name &&
    response.uid === detail.uid &&
    response.resourceVersion === detail.resourceVersion
  );
}
