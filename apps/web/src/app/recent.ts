import type { ResourceRecentItem } from "@k8s-dashboard/contracts";

/**
 * 최근 항목 로컬 저장 (ADR 0023 결정 7)
 * --------------------------------------------------------------------------
 * 브라우저는 **UID와 이동 경로만** 들고 있습니다. 화면에 보일 제목(kind·이름)은
 * 매번 서버가 다시 정합니다 — 그래야 권한이 사라졌거나 같은 이름의 다른 객체로
 * 교체된 항목이 오래된 제목으로 남지 않습니다.
 *
 * 저장 규칙
 * - **버전이 붙습니다.** 형식을 바꾸면 옛 값을 해석하려 들지 않고 버립니다.
 * - **파싱은 방어적입니다.** localStorage는 사용자가 편집할 수 있고 다른 탭·
 *   확장이 덮어쓸 수 있습니다. 모양이 어긋나면 예외가 아니라 빈 목록입니다.
 * - **유계입니다.** 브라우저 전체로 20개, 세그먼트 길이도 서버 상한 안으로 자릅니다.
 *   저장소가 커져서 다른 기능이 QuotaExceeded로 죽는 일이 없어야 합니다.
 * - **클러스터가 신원의 일부입니다.** 목록 하나에 여러 클러스터가 섞여 살지만,
 *   읽고 보내는 것은 언제나 활성 클러스터의 참조뿐입니다. 클러스터를 신원에서
 *   빼면 다른 클러스터의 같은 UID가 같은 객체로 보이고, 클러스터를 바꾼 순간
 *   옛 클러스터의 참조가 새 클러스터 엔드포인트로 나갑니다.
 * - **서버로 가는 ref에는 클러스터가 들어가지 않습니다.** 엔드포인트 경로가 이미
 *   클러스터를 소유합니다.
 */

/** 저장 키. 버전이 바뀌면 키도 바뀌어 옛 값이 자동으로 무시됩니다. */
const STORAGE_KEY = "k8s-dashboard.recent.v1";

/** 서버 계약과 같은 상한입니다. (resourcecatalog.MaxRecentRefs) */
export const MAX_RECENT = 20;

/**
 * 요청 하나의 **request target** 상한.
 *
 * 프록시가 보는 것은 origin-form request target, 즉 `pathname + "?" + query`입니다
 * (scheme·host는 여기 들어가지 않습니다). 그래서 ref 조각만 재는 것으로는 부족하고
 * **클러스터 경로·`?`·모든 `ref=`와 `&`·apiGet이 실제로 덧붙이는 파라미터까지**
 * 함께 셉니다.
 *
 * 서버 상한은 8KiB지만 웹은 **6KiB에서 나눕니다.** 프록시·CDN이 붙는 배포에서
 * 8KiB 직전까지 채워 보내면 우리 잘못이 아닌 이유로 414가 납니다.
 */
export const MAX_RECENT_REQUEST_TARGET_BYTES = 6 << 10;

/** 인코딩된 참조 하나의 길이 상한. (resourcecatalog.MaxRecentRefLen) */
const MAX_REF_LEN = 1024;

/** 참조 인코딩 버전과 구분자. 서버 `EncodeRecentRef`와 같은 형식입니다. */
const REF_VERSION = "1";
const REF_SEP = "\x1f";

/** 참조가 실리는 쿼리 파라미터. 서버가 `ref`를 반복해서 읽습니다. */
export const REF_KEY = "ref";

/** core group의 경로 표기. 서버 `resourcecatalog.CoreGroupAlias`와 같습니다. */
const CORE_GROUP = "core";

/** namespace·name·UID의 문자 집합. 서버 `safeCursorSegment`와 같습니다. */
const SAFE_SEGMENT = /^[A-Za-z0-9._:-]*$/;

/** Kubernetes DNS1123 label / DNS1035 label. 서버 `ValidateGVRSegments`와 같은 규칙입니다. */
const DNS1123_LABEL = /^[a-z0-9]([-a-z0-9]*[a-z0-9])?$/;
const DNS1035_LABEL = /^[a-z]([-a-z0-9]*[a-z0-9])?$/;

/** 브라우저가 들고 있는 참조 하나. 제목은 담지 않습니다 — 서버가 정합니다. */
export interface RecentRef {
  /** 이 참조가 속한 클러스터. 신원의 일부이고 서버 ref에는 실리지 않습니다. */
  clusterId: string;
  /** core group은 "core"입니다. 서버 응답 값을 그대로 보관합니다. */
  group: string;
  version: string;
  resource: string;
  namespace: string;
  name: string;
  uid: string;
}

/** 저장 형식. 배열이 아니라 봉투인 이유는 버전을 함께 싣기 위해서입니다. */
interface RecentEnvelope {
  v: 1;
  items: RecentRef[];
}

/* ── 검증 ───────────────────────────────────────────────────────────────── */

/**
 * 값이 **실제 문자열**이고 서버가 받는 문자 집합 안인지.
 *
 * `String(value)`로 강제 변환하지 않습니다. 숫자 `42`나 배열 `["a"]`를 문자열로
 * 바꿔서 통과시키면 저장소가 오염된 채로 정상처럼 보이고, 그 값이 그대로
 * 인코딩되어 서버에는 400으로 도착합니다. 모양이 어긋난 값은 고쳐 쓰지 않고 버립니다.
 */
function isSafe(value: unknown, max: number): value is string {
  return typeof value === "string" && value.length <= max && SAFE_SEGMENT.test(value);
}

function isSafeNonEmpty(value: unknown, max: number): value is string {
  return isSafe(value, max) && value.length > 0;
}

/** DNS1123 subdomain. label마다 1..63자, 전체 253자입니다. */
function isDNS1123Subdomain(value: unknown): value is string {
  if (typeof value !== "string" || value.length === 0 || value.length > 253) return false;
  return value.split(".").every((label) => label.length > 0 && label.length <= 63 && DNS1123_LABEL.test(label));
}

/** DNS1035 label. 글자로 시작해야 하고 63자 이하입니다. */
function isDNS1035Label(value: unknown): value is string {
  return typeof value === "string" && value.length > 0 && value.length <= 63 && DNS1035_LABEL.test(value);
}

/**
 * 모양이 어긋난 항목은 예외가 아니라 제외입니다.
 *
 * GVR 판정을 서버 `ValidateGVRSegments`와 맞춥니다. 느슨하게 두면 서버가 400을
 * 줄 참조가 같은 요청에 섞여 나가고, **그 배치의 정상 참조까지 함께 400**을
 * 받습니다. 한 항목의 오염이 나머지를 못 쓰게 만들면 안 됩니다.
 */
export function isValidRef(value: unknown): value is RecentRef {
  if (!value || typeof value !== "object" || Array.isArray(value)) return false;
  const r = value as Record<string, unknown>;
  return (
    isSafeNonEmpty(r.clusterId, 253) &&
    (r.group === CORE_GROUP || isDNS1123Subdomain(r.group)) &&
    isDNS1035Label(r.version) &&
    isDNS1035Label(r.resource) &&
    isSafe(r.namespace, 63) &&
    isSafeNonEmpty(r.name, 253) &&
    isSafeNonEmpty(r.uid, 64)
  );
}

/** 같은 객체인지. 신원은 이름이 아니라 **클러스터 + GVR + UID**입니다. */
function identityOf(ref: RecentRef): string {
  return `${ref.clusterId}\u0000${ref.group}/${ref.version}/${ref.resource}/${ref.uid}`;
}

/** 서버 응답 항목의 GVR+UID 부분. 클러스터는 요청 경로가 이미 정했습니다. */
function objectKey(item: { group: string; version: string; resource: string; uid: string }): string {
  return `${item.group}/${item.version}/${item.resource}/${item.uid}`;
}

/* ── 저장소 ─────────────────────────────────────────────────────────────── */

function storage(): Storage | null {
  try {
    return window.localStorage;
  } catch {
    /* private 모드·정책 차단에서는 저장 없이 동작합니다. 기능이 죽지는 않습니다. */
    return null;
  }
}

/** 저장된 참조 전부(모든 클러스터). 어떤 이유로든 해석되지 않으면 빈 목록입니다. */
export function loadAllRecent(): RecentRef[] {
  const store = storage();
  if (!store) return [];
  let raw: string | null = null;
  try {
    raw = store.getItem(STORAGE_KEY);
  } catch {
    return [];
  }
  if (!raw || raw.length > 64 << 10) return [];
  let parsed: unknown;
  try {
    parsed = JSON.parse(raw);
  } catch {
    return [];
  }
  if (!parsed || typeof parsed !== "object") return [];
  const env = parsed as Partial<RecentEnvelope>;
  if (env.v !== 1 || !Array.isArray(env.items)) return [];
  const out: RecentRef[] = [];
  for (const item of env.items) {
    if (out.length >= MAX_RECENT) break;
    if (!isValidRef(item)) continue;
    out.push({
      clusterId: item.clusterId,
      group: item.group,
      version: item.version,
      resource: item.resource,
      namespace: item.namespace,
      name: item.name,
      uid: item.uid,
    });
  }
  return out;
}

/**
 * 활성 클러스터의 참조만 최신순으로.
 *
 * 저장은 브라우저 전체로 20개지만 **보내는 것은 이 클러스터 것뿐**입니다.
 * 다른 클러스터의 참조를 이 엔드포인트로 보내면 남의 클러스터 UID를 물어보는 셈입니다.
 */
export function loadRecent(clusterId: string): RecentRef[] {
  if (!clusterId) return [];
  return loadAllRecent().filter((r) => r.clusterId === clusterId);
}

function saveRecent(items: RecentRef[]): void {
  const store = storage();
  if (!store) return;
  const env: RecentEnvelope = { v: 1, items: items.slice(0, MAX_RECENT) };
  try {
    store.setItem(STORAGE_KEY, JSON.stringify(env));
  } catch {
    /* 용량 초과·정책 차단은 조용히 넘깁니다. 최근 목록 때문에 화면이 죽지 않습니다. */
  }
}

/**
 * 항목 하나를 맨 앞으로 올립니다(있으면 갱신, 없으면 추가).
 * 브라우저 전체로 20개를 넘으면 가장 오래된 것부터 떨어집니다.
 *
 * 반환값은 **그 클러스터의** 목록입니다.
 */
export function rememberRecent(ref: RecentRef): RecentRef[] {
  /* 모양이 어긋난 값은 저장하지 않습니다 — 서버가 400을 줄 참조를 남길 이유가 없습니다. */
  if (!isValidRef(ref)) return [];
  const id = identityOf(ref);
  const rest = loadAllRecent().filter((r) => identityOf(r) !== id);
  const next = [ref, ...rest].slice(0, MAX_RECENT);
  saveRecent(next);
  return next.filter((r) => r.clusterId === ref.clusterId);
}

/**
 * 이번에 **물어본 참조들** 중 서버가 해석하지 못한 것만 지웁니다.
 *
 * 요청 스냅샷을 함께 받는 이유는 두 가지입니다. 첫째, 다른 클러스터의 참조는
 * 이번 응답으로 판단할 수 없으므로 손대면 안 됩니다. 둘째, 요청을 보낸 뒤 다른
 * 탭이 새로 넣은 항목은 응답에 없는 게 당연하므로 지우면 방금 본 것을 잃습니다.
 * 그래서 "응답에 없는 전부"가 아니라 "물어봤는데 못 받은 것"만 제거합니다.
 */
export function pruneRecent(requested: readonly RecentRef[], resolved: readonly ResourceRecentItem[]): void {
  if (requested.length === 0) return;
  const alive = new Set(resolved.map(objectKey));
  const dead = new Set(requested.filter((r) => !alive.has(objectKey(r))).map(identityOf));
  if (dead.size === 0) return;
  saveRecent(loadAllRecent().filter((r) => !dead.has(identityOf(r))));
}

export function clearRecent(): void {
  const store = storage();
  try {
    store?.removeItem(STORAGE_KEY);
  } catch {
    /* 지우지 못해도 화면은 계속 동작합니다. */
  }
}

/* ── 참조 인코딩 ─────────────────────────────────────────────────────────── */

/** base64url(padding 없음). 서버 `base64.RawURLEncoding`과 같은 알파벳입니다. */
function base64url(input: string): string {
  return btoa(input).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
}

/**
 * 서버 `EncodeRecentRef`와 **같은 형식**입니다.
 * `version ␟ group/version/resource ␟ namespace ␟ name ␟ uid`를 base64url로 감쌉니다.
 *
 * `clusterId`는 들어가지 않습니다 — 엔드포인트 경로가 이미 클러스터를 소유합니다.
 * 세그먼트는 전부 ASCII(Kubernetes 이름 규칙)이므로 btoa로 충분합니다.
 */
export function encodeRecentRef(ref: RecentRef): string {
  const raw = [
    REF_VERSION,
    `${ref.group}/${ref.version}/${ref.resource}`,
    ref.namespace,
    ref.name,
    ref.uid,
  ].join(REF_SEP);
  return base64url(raw);
}

/* ── 요청 크기 ──────────────────────────────────────────────────────────── */

/** UTF-8 바이트 수. 상한은 문자 수가 아니라 바이트로 정의되어 있습니다. */
export function utf8Bytes(value: string): number {
  return new TextEncoder().encode(value).length;
}

/**
 * 한 요청이 실제로 만들 request target의 고정 부분.
 *
 * `pathname`은 이미 인코딩된 경로이고, `extraParams`는 `apiGet`이 **실제로**
 * 덧붙이는 파라미터입니다(현재는 mock 시나리오). 여기 빠진 것이 있으면 우리가
 * 6144로 재고 브라우저는 그보다 큰 요청을 보냅니다.
 */
export interface RecentRequestTarget {
  pathname: string;
  extraParams?: ReadonlyArray<readonly [string, string]>;
}

/** 참조 하나가 혼자서도 상한을 넘을 때. 잘라 보내면 서버가 다른 뜻으로 읽습니다. */
export class RecentRequestTooLargeError extends Error {
  constructor(readonly encodedLength: number) {
    super(`recent request target exceeds ${MAX_RECENT_REQUEST_TARGET_BYTES} bytes`);
    this.name = "RecentRequestTooLargeError";
  }
}

/**
 * 이 요청이 브라우저에서 실제로 만들 origin-form request target.
 *
 * **`apiGet`과 같은 순서로 같은 객체를 씁니다** — `new URL(path, origin)`을 만들고
 * 파라미터를 `searchParams.set`으로 얹은 뒤 `pathname + search`를 취합니다.
 * `encodeURIComponent`로 어림잡지 않습니다: `URLSearchParams`는 `~`를 `%7E`로
 * 늘리지만 `encodeURIComponent`는 그대로 둡니다. 그 차이만큼 우리는 작게 재고
 * 브라우저는 크게 보냅니다.
 */
export function recentRequestTarget(
  pathname: string,
  encodedRefs: readonly string[],
  extraParams: ReadonlyArray<readonly [string, string]> = [],
): string {
  const query = encodedRefs.map((value) => `${REF_KEY}=${value}`).join("&");
  const url = new URL(query ? `${pathname}?${query}` : pathname, window.location.origin);
  for (const [key, value] of extraParams) url.searchParams.set(key, value);
  return url.pathname + url.search;
}

/**
 * 요청을 나눕니다. 한 덩어리는 **참조 20개 이하이면서 request target 6KiB 이하**입니다.
 *
 * 후보마다 실제 URL을 만들어 재므로 산술 근사가 어긋날 여지가 없습니다.
 * 참조가 하나도 없는 0건 probe까지 같은 자로 먼저 재고, 그것조차 들어가지 않으면
 * **요청을 하나도 만들지 않고** 실패합니다 — 상한을 넘는 요청을 쏘아 414를 받는
 * 것보다 왜 못 보냈는지 말하는 편이 정직합니다.
 *
 * 나눈 순서가 곧 원래 순서이므로, 각 응답을 순서대로 이어 붙이면 원래 순서가 됩니다.
 */
export function chunkRecentRefs(refs: readonly RecentRef[], target: RecentRequestTarget): string[][] {
  const extra = target.extraParams ?? [];
  const fits = (encoded: readonly string[]) =>
    utf8Bytes(recentRequestTarget(target.pathname, encoded, extra)) <= MAX_RECENT_REQUEST_TARGET_BYTES;

  /* 0건 probe도 못 보내는 경로라면 어떤 요청도 보낼 수 없습니다. */
  if (!fits([])) throw new RecentRequestTooLargeError(0);

  const chunks: string[][] = [];
  let current: string[] = [];

  for (const ref of refs) {
    /* 오염된 항목은 인코딩 전에 걸러냅니다 — 하나 때문에 같은 배치의 정상 참조가
       400을 받으면 안 됩니다. */
    if (!isValidRef(ref)) continue;
    const encoded = encodeRecentRef(ref);
    if (encoded.length > MAX_REF_LEN) continue; // 서버가 400을 줄 참조는 보내지 않습니다.

    if ((current.length >= MAX_RECENT || !fits([...current, encoded])) && current.length > 0) {
      chunks.push(current);
      current = [];
    }
    if (!fits([encoded])) throw new RecentRequestTooLargeError(encoded.length);
    current.push(encoded);
  }
  if (current.length > 0) chunks.push(current);
  return chunks;
}
