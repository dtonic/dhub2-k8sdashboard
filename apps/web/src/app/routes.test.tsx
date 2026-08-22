import { cleanup, render, screen } from "@testing-library/react";
import { MemoryRouter, Navigate, Route, Routes } from "react-router-dom";
import { afterEach, describe, expect, it, vi } from "vitest";

/* 이 프로젝트 vitest 설정은 globals가 없어 RTL 자동 cleanup이 걸리지 않습니다. */
afterEach(cleanup);

/* 화면 구현이 아니라 **경로 배선**을 봅니다. 실제 화면을 그리면 데이터 훅과
   차트까지 끌려와 이 테스트가 무엇을 지키는지 흐려집니다. */
vi.mock("@/features/overview/ClusterOverview", () => ({ ClusterOverview: () => <div>screen:overview</div> }));
vi.mock("@/features/nodes/NodesView", () => ({ NodesView: () => <div>screen:nodes</div> }));
vi.mock("@/features/drill/NamespaceList", () => ({ NamespaceList: () => <div>screen:namespaces</div> }));
vi.mock("@/features/drill/NamespaceDetail", () => ({ NamespaceDetail: () => <div>screen:namespace-detail</div> }));
vi.mock("@/features/drill/WorkloadDetail", () => ({ WorkloadDetail: () => <div>screen:workload-detail</div> }));
vi.mock("@/features/drill/PodDetail", () => ({ PodDetail: () => <div>screen:pod-detail</div> }));
vi.mock("@/features/logs/LogsExplorer", () => ({ LogsExplorer: () => <div>screen:logs</div> }));
vi.mock("@/features/topology/TopologyView", () => ({ TopologyView: () => <div>screen:topology</div> }));
vi.mock("@/features/alerts/AlertsView", () => ({ AlertsView: () => <div>screen:alerts</div> }));
vi.mock("@/features/resources/ResourcesView", () => ({ ResourcesView: () => <div>screen:resources</div> }));
vi.mock("@/features/dashboards/DashboardView", () => ({ DashboardView: () => <div>screen:dashboard</div> }));
vi.mock("@/features/dashboard-builder/DashboardBuilder", () => ({
  DashboardBuilderList: () => <div>screen:builder-list</div>,
  DashboardBuilderEditor: () => <div>screen:builder-editor</div>,
}));
/* ManageView는 kind prop이 계약입니다 — /deployments와 /secrets가 같은 컴포넌트를
   서로 다른 kind로 연다는 사실을 화면 텍스트로 드러냅니다. */
vi.mock("@/features/manage/ManageView", () => ({
  ManageView: ({ kind }: { kind: string }) => <div>screen:manage:{kind}</div>,
}));

import { APP_ROUTES, NAV_GROUPS, navRoutes, navRoutesInGroup, routeAllowed } from "./routes";

/** main.tsx가 하는 것과 **같은 배선**입니다. catch-all만 레지스트리 밖입니다. */
function renderAt(path: string) {
  return render(
    <MemoryRouter initialEntries={[path]}>
      <Routes>
        {APP_ROUTES.map((route) => (
          <Route key={route.id} path={route.path} element={route.element} />
        ))}
        <Route path="*" element={<Navigate to="/" replace />} />
      </Routes>
    </MemoryRouter>,
  );
}

/** 이 슬라이스 이전에 존재하던 경로 전부. 하나도 사라지면 안 됩니다. */
const PRIOR_PATHS = [
  "/",
  "/nodes",
  "/namespaces",
  "/namespaces/:namespace",
  "/workloads/:kind/:name",
  "/pods/:name",
  "/topology",
  "/logs",
  "/alerts",
  "/resources",
  "/deployments",
  "/secrets",
  "/dashboards/:id",
  "/dashboard-builder",
  "/dashboard-builder/:id",
];

describe("라우트 레지스트리", () => {
  it("이전에 있던 경로를 하나도 잃지 않고, 중복도 없습니다", () => {
    const paths = APP_ROUTES.map((r) => r.path);
    for (const prior of PRIOR_PATHS) {
      expect(paths).toContain(prior);
    }
    expect(paths).toHaveLength(PRIOR_PATHS.length);
    expect(new Set(paths).size).toBe(paths.length);
    expect(new Set(APP_ROUTES.map((r) => r.id)).size).toBe(APP_ROUTES.length);
  });

  it("catch-all은 레지스트리 밖에 정확히 하나만 있습니다", () => {
    /* 레지스트리에는 와일드카드가 없어야 합니다 — 있으면 그 뒤 라우트가 죽습니다. */
    expect(APP_ROUTES.filter((r) => r.path.includes("*"))).toHaveLength(0);
    /* 그리고 알 수 없는 경로는 홈으로 되돌아옵니다(= catch-all이 살아 있습니다). */
    renderAt("/definitely-not-a-route");
    expect(screen.getByText("screen:overview")).toBeInTheDocument();
  });

  it.each([
    ["/", "screen:overview"],
    ["/nodes", "screen:nodes"],
    ["/namespaces", "screen:namespaces"],
    ["/namespaces/payments", "screen:namespace-detail"],
    ["/workloads/Deployment/payments-api", "screen:workload-detail"],
    ["/pods/payments-api-abc", "screen:pod-detail"],
    ["/topology", "screen:topology"],
    ["/logs", "screen:logs"],
    ["/alerts", "screen:alerts"],
    ["/resources", "screen:resources"],
    ["/dashboards/cluster", "screen:dashboard"],
    ["/dashboard-builder", "screen:builder-list"],
    ["/dashboard-builder/draft-1", "screen:builder-editor"],
  ])("%s는 그대로 %s를 엽니다", (path, expected) => {
    renderAt(path);
    expect(screen.getByText(expected)).toBeInTheDocument();
  });

  it("/deployments와 /secrets는 서로 다른 kind의 ManageView로 남습니다", () => {
    renderAt("/deployments");
    expect(screen.getByText("screen:manage:deployments")).toBeInTheDocument();
    cleanup();
    renderAt("/secrets");
    expect(screen.getByText("screen:manage:secrets")).toBeInTheDocument();
  });

  it("/resources는 리다이렉트가 아니라 별도 화면입니다", () => {
    renderAt("/resources");
    expect(screen.getByText("screen:resources")).toBeInTheDocument();
    expect(screen.queryByText("screen:manage:deployments")).not.toBeInTheDocument();
    expect(screen.queryByText("screen:overview")).not.toBeInTheDocument();
  });

  it("루트만 end 의미를 갖습니다", () => {
    expect(APP_ROUTES.find((r) => r.path === "/")?.end).toBe(true);
    expect(APP_ROUTES.filter((r) => r.end).map((r) => r.path)).toEqual(["/"]);
  });
});

describe("capability 필터", () => {
  const none = { canExploreResources: false, canManageWorkloads: false };
  const all = { canExploreResources: true, canManageWorkloads: true };

  it("관측 화면은 capability와 무관하게 나옵니다", () => {
    const observe = navRoutesInGroup("observe", none).map((r) => r.path);
    expect(observe).toEqual(["/", "/nodes", "/namespaces", "/topology", "/logs", "/alerts"]);
  });

  it("Resources는 canExploreResources로만, 관리는 canManageWorkloads로만 열립니다", () => {
    expect(navRoutesInGroup("resources", none)).toHaveLength(0);
    expect(navRoutesInGroup("manage", none)).toHaveLength(0);
    expect(navRoutesInGroup("resources", all).map((r) => r.path)).toEqual(["/resources"]);
    expect(navRoutesInGroup("manage", all).map((r) => r.path)).toEqual(["/deployments", "/secrets"]);

    const explorerOnly = { canExploreResources: true, canManageWorkloads: false };
    expect(navRoutesInGroup("resources", explorerOnly).map((r) => r.path)).toEqual(["/resources"]);
    expect(navRoutesInGroup("manage", explorerOnly)).toHaveLength(0);
  });

  it("파라미터가 필요한 상세 화면은 목적지 목록에 없습니다", () => {
    const destinations = navRoutes(all).map((r) => r.path);
    expect(destinations.some((p) => p.includes(":"))).toBe(false);
    expect(destinations).toContain("/dashboard-builder");
    expect(destinations).not.toContain("/dashboard-builder/:id");
  });

  it("routeAllowed는 서버 capability 이름을 그대로 씁니다", () => {
    const resources = APP_ROUTES.find((r) => r.id === "resources")!;
    expect(routeAllowed(resources, none)).toBe(false);
    expect(routeAllowed(resources, { ...none, canExploreResources: true })).toBe(true);
  });

  it("nav 그룹 순서와 제목이 기존과 같습니다", () => {
    expect(NAV_GROUPS.map((g) => g.id)).toEqual(["observe", "resources", "manage", "custom"]);
    expect(NAV_GROUPS.map((g) => g.title)).toEqual(["관측", "리소스", "관리", "Custom"]);
  });
});
