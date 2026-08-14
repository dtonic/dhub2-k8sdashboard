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
  AlertListResponse,
  LogLevel,
  LogLine,
  LogSearchResponse,
  NamespaceDetailResponse,
  NamespaceListResponse,
  PodDetailResponse,
  RangeKey,
  Section,
  TopologyEdgeSeriesResponse,
  TopologyResponse,
  WorkloadDetailResponse,
  WorkloadKind,
} from "@k8s-dashboard/contracts";
import { buildOverview, EVENTS, NOW_MS, rangeWindow, SCOPE, type Scenario } from "./data";
import { afterCursor, encodeCursor, logCorpus } from "./logs";
import { edgeSeries, topologyGraph, topologyUnhealthy } from "./topology";
import { alertList, GROUPING_RULE } from "./alerts";
import {
  containersOf,
  NAMESPACE_NAMES,
  namespaceSummary,
  ownerChainOf,
  podsOf,
  scopedTrends,
  workloadsOf,
} from "./drilldown";
import { dashboardBuilderHandlers } from "./dashboard-builder";

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
  ...dashboardBuilderHandlers,
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
    /* E2E 전용 slow 시나리오는 range 전환 시 fetch abort 전파를 관찰할 시간을 확보합니다. */
    const slow = new URL(request.url).searchParams.get("scenario") === "slow";
    await delay(slow ? 1_500 : 220);
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

  /* ── Logs Explorer (이슈 #16) ─────────────────────────────────────────── */
  http.get("/api/v1/clusters/:clusterId/logs", async ({ request, params }) => {
    const clusterId = String(params.clusterId);
    const url = new URL(request.url);
    const ns = url.searchParams.get("ns") ?? "";
    const auth = authorize(clusterId, ns || undefined);
    if (!auth.ok) {
      await delay(60);
      return denied(auth.message);
    }

    /* from/to가 오면 그대로, 없으면 range로 계산합니다. 차트 구간 선택이 from/to를 씁니다. */
    const w = rangeWindow(rangeOf(request));
    const from = Number(url.searchParams.get("from") ?? Date.parse(w.from));
    const to = Number(url.searchParams.get("to") ?? Date.parse(w.to));
    const levels = (url.searchParams.get("levels") ?? "").split(",").filter(Boolean) as LogLevel[];
    const workload = url.searchParams.get("workload") ?? "";
    const podUid = url.searchParams.get("podUid") ?? "";
    const container = url.searchParams.get("container") ?? "";
    const q = (url.searchParams.get("q") ?? "").trim().toLowerCase();
    const cursor = url.searchParams.get("cursor") ?? "";

    const PAGE = 100;
    /* 서버가 결과 상한을 강제합니다. 브라우저가 요구한다고 무한히 주지 않습니다. (README §11) */
    const MAX_LINES = 5000;

    /* 히스토그램과 facet은 **레벨 필터를 적용하기 전** 집합으로 계산합니다.
       그래야 "WARN을 켜면 몇 줄이 더 보이는가"를 미리 알 수 있습니다.
       레벨 필터까지 반영하면 켜지지 않은 레벨의 카운트가 항상 0으로 보입니다. */
    const base = logCorpus().filter(
      (l) =>
        l.t >= from &&
        l.t <= to &&
        (!ns || l.namespace === ns) &&
        (!workload || l.workloadName === workload) &&
        (!podUid || l.podUid === podUid) &&
        (!container || l.containerName === container) &&
        (!q || l.message.toLowerCase().includes(q)),
    );

    let matched = levels.length ? base.filter((l) => levels.includes(l.level)) : base;

    const truncated = matched.length > MAX_LINES;
    if (truncated) matched = matched.slice(0, MAX_LINES);

    const start = cursor ? afterCursor(matched, cursor) : 0;
    const page = matched.slice(start, start + PAGE);
    const last = page[page.length - 1];
    const next = start + PAGE < matched.length && last ? encodeCursor(last) : null;

    /* 히스토그램·facet은 페이지가 아니라 매칭 전체 기준입니다. */
    const buckets = 60;
    const width = Math.max(1, Math.round((to - from) / buckets));
    const hist = Array.from({ length: buckets }, (_, i) => ({
      t: from + i * width,
      counts: { ERROR: 0, WARN: 0, INFO: 0, DEBUG: 0 } as Record<LogLevel, number>,
    }));
    const countBy = <K extends string>(get: (l: LogLine) => K | undefined) => {
      const m = new Map<K, number>();
      for (const l of base) {
        const k = get(l);
        if (k) m.set(k, (m.get(k) ?? 0) + 1);
      }
      return m;
    };
    for (const l of base) {
      const i = Math.min(buckets - 1, Math.max(0, Math.floor((l.t - from) / width)));
      hist[i]!.counts[l.level] += 1;
    }

    const podCounts = countBy((l) => l.podUid);
    const podName = new Map(base.map((l) => [l.podUid, l.podName]));
    const wlKind = new Map(base.map((l) => [l.workloadName!, l.workloadKind!]));

    await delay(cursor ? 160 : 260);
    const body: LogSearchResponse = {
      lines: page.length ? ok(page) : empty(),
      cursor: { next, pageSize: PAGE },
      histogram: ok(hist),
      events: ok(EVENTS.filter((e) => (!ns || e.namespace === ns))),
      facets: ok({
        workloads: [...countBy((l) => l.workloadName).entries()]
          .map(([name, count]) => ({ name, kind: wlKind.get(name)!, count }))
          .sort((a, b) => b.count - a.count),
        pods: [...podCounts.entries()]
          .map(([uid, count]) => ({ uid, name: podName.get(uid)!, count }))
          .sort((a, b) => b.count - a.count),
        containers: [...countBy((l) => l.containerName).entries()]
          .map(([name, count]) => ({ name, count }))
          .sort((a, b) => b.count - a.count),
      }),
      applied: {
        clusterId,
        namespace: ns || null,
        from: new Date(from).toISOString(),
        to: new Date(to).toISOString(),
        truncated,
        maxLines: MAX_LINES,
      },
      generatedAt: new Date(NOW_MS).toISOString(),
    };
    return HttpResponse.json(body);
  }),

  /* ── Pod Topology ─────────────────────────────────────────────────────── */
  http.get("/api/v1/clusters/:clusterId/topology", async ({ request, params }) => {
    const clusterId = String(params.clusterId);
    const url = new URL(request.url);
    const ns = url.searchParams.get("ns") ?? "";
    const auth = authorize(clusterId, ns || undefined);
    if (!auth.ok) {
      await delay(60);
      return denied(auth.message);
    }
    await delay(260);
    const range = rangeOf(request);
    const scenario = scenarioOf(request);
    const graph = topologyGraph(range);
    const nodes = ns ? graph.nodes.filter((n) => n.namespace === ns) : graph.nodes;
    const ids = new Set(nodes.map((n) => n.id));
    const edges = graph.edges.filter((e) => ids.has(e.from) && ids.has(e.to));
    const unhealthy = topologyUnhealthy().filter((u) => !ns || u.namespace === ns);

    const body: TopologyResponse = {
      ...meta(clusterId, range),
      namespace: ns || null,
      pods: ok({
        total: nodes.length,
        healthy: nodes.filter((n) => n.severity === "healthy").length,
        unhealthy: unhealthy.length,
        unhealthyList: unhealthy,
      }),
      graph:
        scenario === "degraded"
          ? {
              status: "degraded",
              source: "greptimedb",
              reason: "통신 메트릭 일부 누락 · 마지막 성공 값 표시",
              observedAt: new Date(NOW_MS - 7 * 60 * 1000).toISOString(),
              data: { nodes, edges },
            }
          : ok({ nodes, edges }),
    };
    return HttpResponse.json(body);
  }),

  http.get("/api/v1/clusters/:clusterId/topology/edges/:edgeId/series", async ({ request, params }) => {
    const auth = authorize(String(params.clusterId));
    if (!auth.ok) {
      await delay(60);
      return denied(auth.message);
    }
    await delay(200);
    const range = rangeOf(request);
    const body: TopologyEdgeSeriesResponse = {
      edgeId: String(params.edgeId),
      range: meta(String(params.clusterId), range).range,
      generatedAt: new Date(NOW_MS).toISOString(),
      series: ok(edgeSeries(String(params.edgeId), range)),
    };
    return HttpResponse.json(body);
  }),

  /* ── Alerts (이슈 #17) ────────────────────────────────────────────────── */
  http.get("/api/v1/clusters/:clusterId/alerts", async ({ request, params }) => {
    const clusterId = String(params.clusterId);
    const url = new URL(request.url);
    const ns = url.searchParams.get("ns") ?? "";
    const auth = authorize(clusterId, ns || undefined);
    if (!auth.ok) {
      await delay(60);
      return denied(auth.message);
    }
    await delay(200);
    const range = rangeOf(request);
    const scenario = scenarioOf(request);
    const { firing, resolved, counts } = alertList(range, ns);

    /* Alert backend 장애는 **이 섹션만** 죽입니다. 화면 전체를 실패시키지 않습니다.
       (이슈 #17 완료 기준) */
    const down = scenario === "degraded";
    const body: AlertListResponse = {
      ...meta(clusterId, range),
      firing: down
        ? { status: "degraded", source: "alertmanager", reason: "Alertmanager 연결 실패 (connection refused)" }
        : firing.length
          ? ok(firing)
          : empty(),
      resolved: down
        ? { status: "degraded", source: "alertmanager", reason: "Alertmanager 연결 실패 (connection refused)" }
        : resolved.length
          ? ok(resolved)
          : empty(),
      counts: down
        ? { status: "degraded", source: "alertmanager", reason: "Alertmanager 연결 실패 (connection refused)" }
        : ok(counts),
      groupingRule: GROUPING_RULE,
    };
    return HttpResponse.json(body);
  }),
];
