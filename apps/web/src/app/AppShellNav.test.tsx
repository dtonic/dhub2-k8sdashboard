import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen, within } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { afterEach, describe, expect, it, vi } from "vitest";
import type { ScopeResponse } from "@k8s-dashboard/contracts";

afterEach(cleanup);

const hooks = vi.hoisted(() => ({ scope: vi.fn() }));

vi.mock("@/api/queries", () => ({
  useScope: () => hooks.scope(),
  /* 팔레트가 같은 모듈에서 가져오는 것들 — nav 테스트에서는 팔레트가 닫혀 있어
     요청이 나가지 않지만, import는 실제로 일어나므로 전부 채워 둡니다. */
  useResourceSearch: () => ({ data: undefined, isLoading: false, isFetching: false, isSuccess: false, error: undefined }),
  useRecentResources: () => ({ data: undefined, isLoading: false, isFetching: false, isSuccess: false, error: undefined }),
  searchQueryUsable: () => false,
  normalizeSearchQuery: (raw: string) => raw.trim(),
  recentRefsKey: () => "",
  searchKeys: {
    search: (clusterId: string, query: string, cursor: string) => ["resource-search", clusterId, query, cursor],
    recent: (clusterId: string, refs: string) => ["resource-recent", clusterId, refs],
  },
  SEARCH_MIN_QUERY: 2,
  SEARCH_MAX_QUERY: 64,
  SEARCH_MAX_RESULTS: 50,
}));
/* nav만 보는 테스트입니다. AppShell은 레지스트리를 통해 모든 화면 모듈을 끌고 오므로
   (차트·지도까지) 화면 자체는 전부 stub으로 대체합니다 — 여기서 검증하는 것은
   "어떤 링크가 어떤 순서로 보이는가"이지 화면 내용이 아닙니다. */
vi.mock("@/features/overview/ClusterOverview", () => ({ ClusterOverview: () => null }));
vi.mock("@/features/nodes/NodesView", () => ({ NodesView: () => null }));
vi.mock("@/features/drill/NamespaceList", () => ({ NamespaceList: () => null }));
vi.mock("@/features/drill/NamespaceDetail", () => ({ NamespaceDetail: () => null }));
vi.mock("@/features/drill/WorkloadDetail", () => ({ WorkloadDetail: () => null }));
vi.mock("@/features/drill/PodDetail", () => ({ PodDetail: () => null }));
vi.mock("@/features/logs/LogsExplorer", () => ({ LogsExplorer: () => null }));
vi.mock("@/features/topology/TopologyView", () => ({ TopologyView: () => null }));
vi.mock("@/features/manage/ManageView", () => ({ ManageView: () => null }));
vi.mock("@/features/resources/ResourcesView", () => ({ ResourcesView: () => null }));
vi.mock("@/features/alerts/AlertsView", () => ({ AlertsView: () => null }));
vi.mock("@/features/dashboards/DashboardView", () => ({ DashboardView: () => null }));
vi.mock("@/features/dashboard-builder/DashboardBuilder", () => ({
  DashboardBuilderList: () => null,
  DashboardBuilderEditor: () => null,
}));

vi.mock("@/app/AuthGate", () => ({ useAuth: () => ({ enabled: false }) }));
vi.mock("@/app/StreamInvalidator", () => ({ StreamInvalidator: () => null }));
vi.mock("@/lib/env", () => ({ usingMockApi: false }));
vi.mock("@/generated/dashboards", () => ({
  embeddedDashboards: [{ id: "cluster", title: "Cluster Dashboard" }],
}));

import { AppShell } from "./AppShell";

const SCOPE = (over: Partial<ScopeResponse> = {}): ScopeResponse => ({
  clusters: [{ id: "prod-seoul", name: "prod-seoul", namespaces: "all", accessible: true }],
  ...over,
});

function renderShell(scope: ScopeResponse, search = "?cluster=prod-seoul") {
  hooks.scope.mockReturnValue({ data: scope, isError: false });
  /* AppShell이 마운트하는 팔레트가 취소를 위해 QueryClient를 봅니다. 데이터 훅은
     위에서 mock했으므로 이 client로는 요청이 나가지 않습니다. */
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={client}>
      <MemoryRouter initialEntries={[`/${search}`]}>
        <Routes>
          <Route element={<AppShell />}>
            <Route path="/" element={<div>main</div>} />
          </Route>
        </Routes>
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

const nav = () => screen.getByRole("navigation", { name: "주요 화면" });

describe("AppShell 좌측 nav", () => {
  it("관측 그룹의 항목과 순서가 기존 그대로입니다", () => {
    renderShell(SCOPE());
    const links = within(nav()).getAllByRole("link");
    const labels = links.map((l) => l.textContent);
    expect(labels.slice(0, 6)).toEqual([
      "Cluster Overview",
      "Nodes",
      "Namespaces",
      "Pod Topology",
      "Logs Explorer",
      "Alerts",
    ]);
  });

  it("capability가 없으면 Resources·관리 그룹이 나오지 않습니다", () => {
    renderShell(SCOPE());
    expect(within(nav()).queryByRole("link", { name: "Resources" })).not.toBeInTheDocument();
    expect(within(nav()).queryByRole("link", { name: "Deployments" })).not.toBeInTheDocument();
    expect(within(nav()).queryByRole("link", { name: "Secrets" })).not.toBeInTheDocument();
    expect(within(nav()).queryByText("리소스")).not.toBeInTheDocument();
    expect(within(nav()).queryByText("관리")).not.toBeInTheDocument();
  });

  it("canExploreResources만 있으면 Resources만 열립니다", () => {
    renderShell(SCOPE({ canExploreResources: true }));
    /* 두 capability는 서로 독립입니다 — Resources가 관리 화면을 딸려 오면 안 됩니다. */
    expect(within(nav()).getByRole("link", { name: "Resources" })).toHaveAttribute(
      "href",
      "/resources?cluster=prod-seoul",
    );
    expect(within(nav()).queryByRole("link", { name: "Deployments" })).not.toBeInTheDocument();
    expect(within(nav()).queryByRole("link", { name: "Secrets" })).not.toBeInTheDocument();
  });

  it("canManageWorkloads가 있으면 Deployments·Secrets가 각자 링크로 남습니다", () => {
    renderShell(SCOPE({ canManageWorkloads: true }));
    /* 경로가 포함되었는지가 아니라 **정확히 무엇으로 이동하는지**를 고정합니다.
       stringContaining은 `/deployments-v2`나 리다이렉트 경유도 통과시킵니다. */
    expect(within(nav()).getByRole("link", { name: "Deployments" })).toHaveAttribute(
      "href",
      "/deployments?cluster=prod-seoul",
    );
    expect(within(nav()).getByRole("link", { name: "Secrets" })).toHaveAttribute(
      "href",
      "/secrets?cluster=prod-seoul",
    );
    /* 반대 방향의 독립도 함께 고정합니다 — 관리 권한이 Resources를 열지 않습니다. */
    expect(within(nav()).queryByRole("link", { name: "Resources" })).not.toBeInTheDocument();
  });

  it("Custom 그룹은 embedded dashboard 먼저, 그다음 Dashboard Builder입니다", () => {
    renderShell(SCOPE());
    const labels = within(nav())
      .getAllByRole("link")
      .map((l) => l.textContent);
    const dashboardAt = labels.indexOf("Cluster Dashboard");
    const builderAt = labels.indexOf("Dashboard Builder");
    expect(dashboardAt).toBeGreaterThan(-1);
    expect(builderAt).toBeGreaterThan(dashboardAt);
  });

  it("공용 파라미터만 링크에 남습니다 — 화면 전용 필터는 새어들지 않습니다", () => {
    renderShell(SCOPE(), "?cluster=prod-seoul&ns=payments&edge=e-1&from=1&q=boom");
    const nodes = within(nav()).getByRole("link", { name: "Nodes" });
    const href = nodes.getAttribute("href") ?? "";
    expect(href).toContain("cluster=prod-seoul");
    expect(href).toContain("ns=payments");
    expect(href).not.toContain("edge=");
    expect(href).not.toContain("from=");
    expect(href).not.toContain("q=");
  });
});
