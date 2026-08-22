import { http, HttpResponse, delay } from "msw";
import type {
  ResourceCatalogResponse,
  ResourceDescriptor,
  ResourceDetailResponse,
  ResourceListItem,
  ResourceListResponse,
  ResourceRecentItem,
  ResourceRecentResponse,
  ResourceSearchItem,
  ResourceSearchResponse,
} from "@k8s-dashboard/contracts";
import { NOW_MS } from "./data";

/**
 * Resource Explorer mock (ADR 0018)
 * --------------------------------------------------------------------------
 * 서버 계약을 그대로 흉내 냅니다 — ready만 목록이 나오고, syncing·unsupported·
 * forbidden·missing은 각각 다른 오류 코드로 답합니다. 다섯 상태가 실제로 다르게
 * 보이는지 화면에서 확인할 수 있어야 합니다.
 *
 * Secret 상세는 서버와 마찬가지로 값이 **없습니다** — data/stringData는 응답에
 * 실리지 않고 redacted 목록으로만 알립니다.
 */

const iso = new Date(NOW_MS).toISOString();

export const RESOURCE_CATALOG: ResourceDescriptor[] = [
  { group: "core", version: "v1", resource: "services", kind: "Service", namespaced: true, verbs: ["get", "list", "watch"], state: "ready", count: 42 },
  { group: "core", version: "v1", resource: "configmaps", kind: "ConfigMap", namespaced: true, verbs: ["get", "list", "watch"], state: "ready", count: 18 },
  { group: "core", version: "v1", resource: "secrets", kind: "Secret", namespaced: true, verbs: ["get", "list", "watch"], state: "ready", count: 7 },
  { group: "storage.k8s.io", version: "v1", resource: "storageclasses", kind: "StorageClass", namespaced: false, verbs: ["get", "list", "watch"], state: "ready", count: 3 },
  { group: "batch", version: "v1", resource: "jobs", kind: "Job", namespaced: true, verbs: ["get", "list", "watch"], state: "syncing", reason: "", count: 0 },
  { group: "networking.k8s.io", version: "v1", resource: "ingresses", kind: "Ingress", namespaced: true, verbs: ["get", "list", "watch"], state: "unsupported", reason: "이 API는 metadata 전용 조회를 지원하지 않습니다", count: 0 },
  { group: "policy", version: "v1", resource: "poddisruptionbudgets", kind: "PodDisruptionBudget", namespaced: true, verbs: ["get"], state: "forbidden", reason: "서버에 이 리소스의 list/watch 권한이 없습니다", count: 0 },
  { group: "autoscaling", version: "v2", resource: "horizontalpodautoscalers", kind: "HorizontalPodAutoscaler", namespaced: true, verbs: [], state: "missing", reason: "클러스터가 이 API를 제공하지 않습니다", count: 0 },
];

const NAMESPACES = ["payments", "search", "media"];

function rowsFor(resource: string, namespaced: boolean): ResourceListItem[] {
  const descriptor = RESOURCE_CATALOG.find((d) => d.resource === resource);
  const count = descriptor?.count ?? 0;
  return Array.from({ length: count }, (_, i) => ({
    ...(namespaced ? { namespace: NAMESPACES[i % NAMESPACES.length] } : {}),
    name: `${resource.replace(/s$/, "")}-${String(i).padStart(3, "0")}`,
    uid: `uid-${resource}-${String(i).padStart(3, "0")}`,
    createdAt: iso,
  })).sort((a, b) => `${a.namespace ?? ""}/${a.name}`.localeCompare(`${b.namespace ?? ""}/${b.name}`));
}

const STATE_ERROR: Record<string, { status: number; code: string; message: string }> = {
  syncing: { status: 503, code: "resource_syncing", message: "리소스 캐시를 동기화하는 중입니다." },
  unsupported: { status: 502, code: "resource_unsupported", message: "이 API는 metadata 전용 조회를 지원하지 않습니다." },
  forbidden: { status: 502, code: "resource_forbidden", message: "서버에 이 리소스의 조회 권한이 없습니다." },
  missing: { status: 404, code: "resource_not_served", message: "클러스터가 이 API를 제공하지 않습니다." },
};

const apiError = (status: number, code: string, message: string) =>
  HttpResponse.json({ code, message, requestId: "mock-request" }, { status });

/* ── 전역 검색 · 최근 항목 (ADR 0023) ───────────────────────────────────────
   서버 계약을 그대로 흉내 냅니다 — 질의는 2..64자이고, 결과에 **status가 없으며**,
   Scope 밖 객체는 애초에 후보에 없습니다. 최근 항목은 해석되지 않는 참조를
   오류가 아니라 **조용한 제거**로 답합니다. */

/** 검색 가능한 후보. ready 상태 리소스에서만 만듭니다. */
function searchCorpus(): ResourceSearchItem[] {
  const out: ResourceSearchItem[] = [];
  for (const d of RESOURCE_CATALOG) {
    if (d.state !== "ready") continue;
    for (const row of rowsFor(d.resource, d.namespaced)) {
      out.push({
        group: d.group,
        version: d.version,
        resource: d.resource,
        kind: d.kind,
        namespaced: d.namespaced,
        ...(row.namespace ? { namespace: row.namespace } : {}),
        name: row.name,
        uid: row.uid,
        matchedField: "name",
      });
    }
  }
  return out;
}

/**
 * mock 전용 label 색인.
 *
 * 서버는 label **값**에도 걸리고 `matchedField: "label"`을 답합니다. 여기서
 * 그 경우를 만들지 않으면 UI의 "Label 일치" 표시가 한 번도 실행되지 않은 채로
 * 남습니다 — 실서버에서 처음 보이는 문구가 됩니다.
 */
function labelValueOf(item: ResourceSearchItem): string {
  return item.namespace ? `team-${item.namespace}` : "team-platform";
}

/** 서버와 같은 규칙 — 이름·namespace·kind·label 값 접두사에 걸립니다. */
function matchOf(item: ResourceSearchItem, q: string): ResourceSearchItem["matchedField"] | null {
  if (item.name.toLowerCase().startsWith(q)) return "name";
  if ((item.namespace ?? "").toLowerCase().startsWith(q)) return "namespace";
  if (item.kind.toLowerCase().startsWith(q)) return "kind";
  if (labelValueOf(item).toLowerCase().startsWith(q)) return "label";
  return null;
}

/** 서버 ValidateGVRSegments와 같은 규칙 — mock도 같은 것을 거절해야 대조가 됩니다. */
const DNS1123_LABEL = /^[a-z0-9]([-a-z0-9]*[a-z0-9])?$/;
const DNS1035_LABEL = /^[a-z]([-a-z0-9]*[a-z0-9])?$/;
const SAFE_SEGMENT = /^[A-Za-z0-9._:-]*$/;

function validGVR(gvr: string): boolean {
  const parts = gvr.split("/");
  if (parts.length !== 3) return false;
  const [group, version, resource] = parts;
  const groupOk =
    group === "core" ||
    (group.length > 0 &&
      group.length <= 253 &&
      group.split(".").every((l) => l.length > 0 && l.length <= 63 && DNS1123_LABEL.test(l)));
  return (
    groupOk &&
    version.length <= 63 &&
    DNS1035_LABEL.test(version) &&
    resource.length <= 63 &&
    DNS1035_LABEL.test(resource)
  );
}

/**
 * 서버 EncodeRecentRef와 같은 형식을 되돌립니다(mock 전용 디코더).
 *
 * 형식뿐 아니라 **세그먼트 규칙까지** 봅니다 — 서버가 400을 줄 참조를 mock이
 * 조용히 받아 주면, 웹이 오염된 참조를 걸러 내는지 아무도 확인하지 못합니다.
 */
function decodeRef(encoded: string): { gvr: string; namespace: string; name: string; uid: string } | null {
  try {
    const padded = encoded.replace(/-/g, "+").replace(/_/g, "/");
    const raw = atob(padded + "=".repeat((4 - (padded.length % 4)) % 4));
    const parts = raw.split("\x1f");
    if (parts.length !== 5 || parts[0] !== "1") return null;
    const [, gvr, namespace, name, uid] = parts;
    if (!validGVR(gvr)) return null;
    if (namespace.length > 63 || !SAFE_SEGMENT.test(namespace)) return null;
    if (name.length === 0 || name.length > 253 || !SAFE_SEGMENT.test(name)) return null;
    if (uid.length === 0 || uid.length > 64 || !SAFE_SEGMENT.test(uid)) return null;
    return { gvr, namespace, name, uid };
  } catch {
    return null;
  }
}

export const resourceHandlers = [
  /* 검색·최근은 목록 라우트(`:group/:version/:resource`)보다 **먼저** 등록합니다 —
     MSW는 배열 순서로 맞추므로 뒤에 두면 목록 핸들러가 먼저 가로챕니다. */
  http.get("/api/v1/clusters/:clusterId/resources/search", async ({ params, request }) => {
    await delay(60);
    const url = new URL(request.url);
    const raw = (url.searchParams.get("q") ?? "").trim();
    if (raw.length < 2 || raw.length > 64) {
      return apiError(400, "invalid_query", "검색어는 2자 이상 64자 이하여야 합니다.");
    }
    const limit = Math.min(Number(url.searchParams.get("limit") ?? "20") || 20, 50);
    const q = raw.toLowerCase();

    const hits: ResourceSearchItem[] = [];
    for (const item of searchCorpus()) {
      const matchedField = matchOf(item, q);
      if (matchedField) hits.push({ ...item, matchedField });
    }
    hits.sort((a, b) => `${a.namespace ?? ""}/${a.name}`.localeCompare(`${b.namespace ?? ""}/${b.name}`));
    const page = hits.slice(0, limit);

    const body: ResourceSearchResponse = {
      clusterId: String(params.clusterId),
      query: q,
      generatedAt: iso,
      observedAt: iso,
      appliedScope: { clusterId: String(params.clusterId), namespaces: "all" },
      items: page,
      truncated: hits.length > page.length,
      /* 시나리오로 "부분 색인"을 재현합니다 — 잘린 검색을 완전한 검색처럼 보여주지 않습니다. */
      degraded: url.searchParams.get("scenario") === "degraded",
      ...(url.searchParams.get("scenario") === "degraded"
        ? { reason: "색인 예산으로 일부 label이 제외되었습니다" }
        : {}),
    };
    return HttpResponse.json(body);
  }),

  http.get("/api/v1/clusters/:clusterId/resources/recent", async ({ params, request }) => {
    await delay(60);
    const url = new URL(request.url);
    if (url.search.length > 8 << 10) {
      return apiError(400, "invalid_filter", "최근 항목 요청이 너무 큽니다. 나눠서 보내세요.");
    }
    const encoded = url.searchParams.getAll("ref");
    if (encoded.length > 20) return apiError(400, "invalid_filter", "참조가 너무 많습니다.");

    const corpus = searchCorpus();
    const items: ResourceRecentItem[] = [];
    for (const value of encoded) {
      const ref = decodeRef(value);
      if (!ref) return apiError(400, "invalid_filter", "참조 형식이 올바르지 않습니다.");
      const hit = corpus.find(
        (c) =>
          `${c.group}/${c.version}/${c.resource}` === ref.gvr &&
          (c.namespace ?? "") === ref.namespace &&
          c.name === ref.name &&
          c.uid === ref.uid,
      );
      /* 해석되지 않는 참조는 오류가 아니라 **조용한 제거**입니다. */
      if (!hit) continue;
      items.push({
        group: hit.group, version: hit.version, resource: hit.resource,
        kind: hit.kind, namespaced: hit.namespaced,
        ...(hit.namespace ? { namespace: hit.namespace } : {}),
        name: hit.name, uid: hit.uid,
      });
    }
    const body: ResourceRecentResponse = {
      clusterId: String(params.clusterId),
      generatedAt: iso,
      appliedScope: { clusterId: String(params.clusterId), namespaces: "all" },
      items,
    };
    return HttpResponse.json(body);
  }),

  http.get("/api/v1/clusters/:clusterId/resources", async ({ params }) => {
    await delay(80);
    const body: ResourceCatalogResponse = {
      clusterId: String(params.clusterId),
      generatedAt: iso,
      refreshedAt: iso,
      degraded: false,
      items: RESOURCE_CATALOG,
    };
    return HttpResponse.json(body);
  }),

  http.get("/api/v1/clusters/:clusterId/resources/:group/:version/:resource", async ({ params, request }) => {
    await delay(80);
    const { group, version, resource } = params as Record<string, string>;
    const descriptor = RESOURCE_CATALOG.find(
      (d) => d.group === group && d.version === version && d.resource === resource,
    );
    if (!descriptor) return apiError(404, "resource_not_allowlisted", "이 리소스는 탐색 대상으로 등록되어 있지 않습니다.");
    const failure = STATE_ERROR[descriptor.state];
    if (failure) return apiError(failure.status, failure.code, failure.message);

    const url = new URL(request.url);
    const limit = Math.min(Number(url.searchParams.get("limit") ?? "50") || 50, 200);
    const ns = url.searchParams.get("ns") ?? "";
    const namePrefix = url.searchParams.get("name") ?? "";
    const cursor = url.searchParams.get("cursor") ?? "";
    const desc = url.searchParams.get("order") === "desc";

    let items = rowsFor(resource, descriptor.namespaced);
    if (ns) items = items.filter((r) => r.namespace === ns);
    if (namePrefix) items = items.filter((r) => r.name.startsWith(namePrefix));
    if (desc) items = [...items].reverse();

    /* 서버와 같은 keyset 의미 — cursor는 마지막으로 본 위치이고 offset이 아닙니다. */
    const start = cursor ? items.findIndex((r) => `${r.namespace ?? ""}/${r.name}` === atob(cursor)) + 1 : 0;
    const page = items.slice(start, start + limit);
    const last = page[page.length - 1];
    const hasMore = start + limit < items.length;

    const body: ResourceListResponse = {
      clusterId: String(params.clusterId),
      group,
      version,
      resource,
      kind: descriptor.kind,
      namespaced: descriptor.namespaced,
      generatedAt: iso,
      observedAt: iso,
      appliedScope: { clusterId: String(params.clusterId), namespaces: ns ? [ns] : "all" },
      items: page,
      ...(hasMore && last ? { nextCursor: btoa(`${last.namespace ?? ""}/${last.name}`) } : {}),
      truncated: hasMore,
      total: items.length,
    };
    return HttpResponse.json(body);
  }),

  http.get("/api/v1/clusters/:clusterId/resources/:group/:version/:resource/object", async ({ params, request }) => {
    await delay(80);
    const { group, version, resource } = params as Record<string, string>;
    const descriptor = RESOURCE_CATALOG.find(
      (d) => d.group === group && d.version === version && d.resource === resource,
    );
    if (!descriptor) return apiError(404, "resource_not_allowlisted", "이 리소스는 탐색 대상으로 등록되어 있지 않습니다.");
    const url = new URL(request.url);
    const namespace = url.searchParams.get("namespace") ?? "";
    const name = url.searchParams.get("name") ?? "";
    const uid = url.searchParams.get("uid") ?? "";
    if (!name || !uid) return apiError(400, "invalid_filter", "name과 uid가 필요합니다.");

    const row = rowsFor(resource, descriptor.namespaced).find((r) => r.name === name);
    if (!row) return apiError(404, "not_found", "목록에 없는 항목입니다.");
    if (row.uid !== uid) return apiError(409, "uid_mismatch", "같은 이름의 다른 객체로 교체되었습니다.");

    const isSecret = resource === "secrets";
    const yaml = [
      `apiVersion: ${group === "core" ? version : `${group}/${version}`}`,
      `kind: ${descriptor.kind}`,
      "metadata:",
      `  name: ${name}`,
      ...(namespace ? [`  namespace: ${namespace}`] : []),
      `  uid: ${uid}`,
      "  resourceVersion: \"4242\"",
      "  labels:",
      "    app.kubernetes.io/name: payments",
      ...(isSecret ? ["type: Opaque"] : ["spec:", "  selector:", "    app.kubernetes.io/name: payments"]),
      "",
    ].join("\n");

    const body: ResourceDetailResponse = {
      clusterId: String(params.clusterId),
      group,
      version,
      resource,
      apiVersion: group === "core" ? version : `${group}/${version}`,
      kind: descriptor.kind,
      ...(namespace ? { namespace } : {}),
      name,
      uid,
      resourceVersion: "4242",
      generatedAt: iso,
      yaml,
      redacted: isSecret ? ["data", "metadata.managedFields"] : ["metadata.managedFields"],
    };
    return HttpResponse.json(body);
  }),
];
