import { http, HttpResponse, delay } from "msw";
import type {
  ResourceCatalogResponse,
  ResourceDescriptor,
  ResourceDetailResponse,
  ResourceListItem,
  ResourceListResponse,
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

export const resourceHandlers = [
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
