/**
 * MSW 핸들러 — 실제 Observability API가 붙기 전까지 UI를 독립 실행합니다.
 *
 * 시나리오는 `?scenario=degraded|forbidden|empty` 쿼리로 바꿉니다.
 * 부분 장애 · 권한 없음 · 빈 결과를 실제로 렌더해 봐야 세 상태가 구분되는지 확인할 수 있습니다.
 *
 * **권한은 서버가 강제합니다.** 접근 불가한 Namespace를 URL로 직접 찍어도
 * 데이터가 아니라 403이 나갑니다. (이슈 #15 완료 기준)
 */
import { http, HttpResponse, delay } from "msw";
import type {
  NamespaceDetailResponse,
  NamespaceListResponse,
  PodDetailResponse,
  RangeKey,
  Section,
  WorkloadDetailResponse,
  WorkloadKind,
} from "@k8s-dashboard/contracts";
import { buildOverview, EVENTS, NOW_MS, rangeWindow, SCOPE, type Scenario } from "./data";
import {
  containersOf,
  NAMESPACE_NAMES,
  namespaceSummary,
  ownerChainOf,
  podsOf,
  scopedTrends,
  workloadsOf,
} from "./drilldown";

const scenarioOf = (req: Request): Scenario => {
  const s = new URL(req.url).searchParams.get("scenario");
  return s === "degraded" || s === "forbidden" || s === "empty" ? s : "default";
};

const rangeOf = (req: Request) => (new URL(req.url).searchParams.get("range") ?? "1h") as RangeKey;

const ok = <T,>(data: T): Section<T> => ({ status: "ok", data, observedAt: new Date(NOW_MS).toISOString() });
const empty = <T,>(): Section<T> => ({ status: "empty", observedAt: new Date(NOW_MS).toISOString() });

const denied = (message: string) => HttpResponse.json({ code: "forbidden", message }, { status: 403 });

/** 클러스터 접근 가능 여부와 Namespace Scope를 한 곳에서 판정합니다. */
function authorize(clusterId: string, namespace?: string) {
  const cluster = SCOPE.clusters.find((c) => c.id === clusterId);
  if (!cluster || !cluster.accessible) {
    return { ok: false as const, message: "이 클러스터에 대한 접근 권한이 없습니다." };
  }
  const allowed = cluster.namespaces === "all" ? NAMESPACE_NAMES : cluster.namespaces;
  if (namespace && !allowed.includes(namespace)) {
    return { ok: false as const, message: `Namespace "${namespace}"에 대한 접근 권한이 없습니다.` };
  }
  return { ok: true as const, cluster, allowed };
}

function meta(clusterId: string, range: RangeKey) {
  const w = rangeWindow(range);
  return {
    clusterId,
    range: { key: range, from: w.from, to: w.to, stepSeconds: w.stepSeconds },
    generatedAt: new Date(NOW_MS).toISOString(),
  };
}

const eventsFor = (predicate: (name: string, ns: string) => boolean) =>
  EVENTS.filter((e) => predicate(e.involvedName, e.namespace));

export const handlers = [
  http.get("/api/v1/scope", async () => {
    await delay(80);
    return HttpResponse.json(SCOPE);
  }),

  /* ── Cluster Overview (이슈 #14) ──────────────────────────────────────── */
  http.get("/api/v1/clusters/:clusterId/overview", async ({ request, params }) => {
    const auth = authorize(String(params.clusterId));
    if (!auth.ok) {
      await delay(60);
      return denied(auth.message);
    }
    await delay(220);
    const body = buildOverview(rangeOf(request), scenarioOf(request));
    return HttpResponse.json({ ...body, clusterId: auth.cluster.id, clusterName: auth.cluster.name });
  }),

  /* ── Namespace 목록 (이슈 #15) ────────────────────────────────────────── */
  http.get("/api/v1/clusters/:clusterId/namespaces", async ({ request, params }) => {
    const clusterId = String(params.clusterId);
    const auth = authorize(clusterId);
    if (!auth.ok) {
      await delay(60);
      return denied(auth.message);
    }
    await delay(180);
    const scenario = scenarioOf(request);
    const list = auth.allowed.map((ns) => namespaceSummary(ns));
    const body: NamespaceListResponse = {
      ...meta(clusterId, rangeOf(request)),
      namespaces:
        scenario === "empty"
          ? empty()
          : scenario === "degraded"
            ? {
                status: "degraded",
                source: "kubernetes",
                reason: "Informer 재동기화 중 · 목록이 최신이 아닐 수 있습니다",
                observedAt: new Date(NOW_MS - 5 * 60 * 1000).toISOString(),
                data: list,
              }
            : ok(list),
    };
    return HttpResponse.json(body);
  }),

  /* ── Namespace 상세 ───────────────────────────────────────────────────── */
  http.get("/api/v1/clusters/:clusterId/namespaces/:namespace", async ({ request, params }) => {
    const clusterId = String(params.clusterId);
    const ns = String(params.namespace);
    const auth = authorize(clusterId, ns);
    if (!auth.ok) {
      await delay(60);
      return denied(auth.message);
    }
    await delay(240);
    const range = rangeOf(request);
    const scenario = scenarioOf(request);
    const body: NamespaceDetailResponse = {
      ...meta(clusterId, range),
      namespace: ns,
      summary: ok(namespaceSummary(ns)),
      workloads: scenario === "empty" ? empty() : ok(workloadsOf(ns)),
      trends:
        scenario === "degraded"
          ? { status: "degraded", source: "greptimedb", reason: "GreptimeDB 응답 없음 (timeout 5s)" }
          : ok(scopedTrends(range, `ns/${ns}`)),
      events:
        scenario === "forbidden"
          ? { status: "forbidden", reason: "cluster.viewer 권한이 필요합니다" }
          : ok(eventsFor((_, e) => e === ns)),
    };
    return HttpResponse.json(body);
  }),

  /* ── Workload 상세 ────────────────────────────────────────────────────── */
  http.get("/api/v1/clusters/:clusterId/workloads/:kind/:name", async ({ request, params }) => {
    const clusterId = String(params.clusterId);
    const url = new URL(request.url);
    const ns = url.searchParams.get("ns") ?? "";
    const auth = authorize(clusterId, ns);
    if (!auth.ok) {
      await delay(60);
      return denied(auth.message);
    }
    const kind = String(params.kind) as WorkloadKind;
    const name = String(params.name);
    const w = workloadsOf(ns).find((x) => x.name === name && x.kind === kind);
    if (!w) {
      await delay(80);
      return HttpResponse.json({ code: "internal", message: "Workload를 찾을 수 없습니다." }, { status: 404 });
    }
    await delay(220);
    const range = rangeOf(request);
    const body: WorkloadDetailResponse = {
      ...meta(clusterId, range),
      namespace: ns,
      workload: ok(w),
      ownerChain: ownerChainOf(w).length ? ok(ownerChainOf(w)) : empty(),
      pods: ok(podsOf(w)),
      trends: ok(scopedTrends(range, `wl/${ns}/${name}`)),
      events: ok(eventsFor((n, e) => e === ns && n.startsWith(name))),
    };
    return HttpResponse.json(body);
  }),

  /* ── Pod 상세 ─────────────────────────────────────────────────────────── */
  http.get("/api/v1/clusters/:clusterId/pods/:name", async ({ request, params }) => {
    const clusterId = String(params.clusterId);
    const url = new URL(request.url);
    const ns = url.searchParams.get("ns") ?? "";
    const auth = authorize(clusterId, ns);
    if (!auth.ok) {
      await delay(60);
      return denied(auth.message);
    }
    const name = String(params.name);
    const wantedUid = url.searchParams.get("uid");

    /* 이름이 같아도 UID가 다르면 다른 인스턴스입니다. UID가 오면 UID를 우선합니다. */
    let found: ReturnType<typeof podsOf>[number] | undefined;
    let owner = undefined as ReturnType<typeof workloadsOf>[number] | undefined;
    for (const w of workloadsOf(ns)) {
      const pods = podsOf(w);
      const hit = wantedUid ? pods.find((p) => p.uid === wantedUid) : pods.find((p) => p.name === name);
      if (hit) {
        found = hit;
        owner = w;
        break;
      }
    }
    if (!found) {
      await delay(80);
      return HttpResponse.json({ code: "internal", message: "Pod를 찾을 수 없습니다." }, { status: 404 });
    }

    await delay(200);
    const range = rangeOf(request);
    const chain = owner ? ownerChainOf(owner) : [];
    const body: PodDetailResponse = {
      ...meta(clusterId, range),
      namespace: ns,
      pod: ok(found),
      ownerChain: owner
        ? ok([
            ...chain.filter((c) => c.uid === found!.owner?.uid || c.current),
            {
              kind: owner.kind,
              name: owner.name,
              uid: owner.ref.workloadUid ?? owner.name,
              current: true,
              pods: owner.replicas.desired,
            },
          ])
        : empty(),
      containers: ok(containersOf(found, owner)),
      trends: ok(scopedTrends(range, `pod/${found.uid}`)),
      events: ok(eventsFor((n, e) => e === ns && n === found!.name)),
    };
    return HttpResponse.json(body);
  }),
];
