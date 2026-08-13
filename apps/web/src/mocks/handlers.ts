/**
 * MSW 핸들러 — 실제 Observability API가 붙기 전까지 UI를 독립 실행합니다.
 *
 * 시나리오는 `?scenario=degraded|forbidden|empty` 쿼리로 바꿉니다.
 * 부분 장애 · 권한 없음 · 빈 결과를 실제로 렌더해 봐야 세 상태가 구분되는지 확인할 수 있습니다.
 */
import { http, HttpResponse, delay } from "msw";
import type { RangeKey } from "@k8s-dashboard/contracts";
import { buildOverview, SCOPE, type Scenario } from "./data";

const scenarioOf = (req: Request): Scenario => {
  const s = new URL(req.url).searchParams.get("scenario");
  return s === "degraded" || s === "forbidden" || s === "empty" ? s : "default";
};

export const handlers = [
  http.get("/api/v1/scope", async () => {
    await delay(80);
    return HttpResponse.json(SCOPE);
  }),

  http.get("/api/v1/clusters/:clusterId/overview", async ({ request, params }) => {
    const url = new URL(request.url);
    const range = (url.searchParams.get("range") ?? "1h") as RangeKey;
    const scenario = scenarioOf(request);

    /* 접근 불가한 클러스터는 화면 전체가 403입니다. 섹션 단위 forbidden과 다릅니다. */
    const cluster = SCOPE.clusters.find((c) => c.id === params.clusterId);
    if (!cluster || !cluster.accessible) {
      await delay(60);
      return HttpResponse.json(
        { code: "forbidden", message: "이 클러스터에 대한 접근 권한이 없습니다." },
        { status: 403 },
      );
    }

    await delay(220);
    const body = buildOverview(range, scenario);
    return HttpResponse.json({ ...body, clusterId: cluster.id, clusterName: cluster.name });
  }),
];
