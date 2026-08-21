import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type { ScopeResponse } from "@k8s-dashboard/contracts";

afterEach(cleanup);

const hooks = vi.hoisted(() => ({ scope: vi.fn() }));

vi.mock("@/api/queries", () => ({
  useScope: () => hooks.scope(),
  useResourceCatalog: () => ({ data: { clusterId: "prod-seoul", generatedAt: "", degraded: false, items: [] }, isLoading: false, error: undefined }),
  useResourceList: () => ({ data: undefined, error: undefined, isLoading: false, isSuccess: false, isFetching: false, isFetchingNextPage: false, hasNextPage: false, fetchNextPage: vi.fn(), refetch: vi.fn() }),
  useResourceObject: () => ({ data: undefined, isLoading: false, error: undefined }),
}));

import { ResourcesView } from "./ResourcesView";

const SCOPE: ScopeResponse = {
  clusters: [{ id: "prod-seoul", name: "prod-seoul", namespaces: "all", accessible: true }],
  canManageWorkloads: true,
  canExploreResources: true,
};

function renderView(scope: ScopeResponse = SCOPE) {
  hooks.scope.mockReturnValue({ data: scope });
  return render(
    <MemoryRouter initialEntries={["/resources?cluster=prod-seoul"]}>
      <ResourcesView />
    </MemoryRouter>,
  );
}

beforeEach(() => hooks.scope.mockReset());

describe("Resources 진입 화면", () => {
  it("Explorer · Deployments · Secrets 세 탭을 tablist로 노출한다", () => {
    renderView();
    const tabs = screen.getAllByRole("tab");
    expect(tabs.map((t) => t.textContent?.replace(" — 전용 화면으로 이동", ""))).toEqual([
      "Explorer",
      "Deployments",
      "Secrets",
    ]);
    expect(screen.getByRole("tablist", { name: "리소스 작업" })).toBeVisible();
  });

  it("선택 상태를 색이 아니라 aria-selected로 전달한다", () => {
    renderView();
    const [explorer, deployments, secrets] = screen.getAllByRole("tab");
    expect(explorer).toHaveAttribute("aria-selected", "true");
    expect(deployments).toHaveAttribute("aria-selected", "false");
    expect(secrets).toHaveAttribute("aria-selected", "false");
    /* roving tabindex — 탭 묶음은 Tab 정지점 하나만 차지합니다. */
    expect(explorer).toHaveAttribute("tabindex", "0");
    expect(deployments).toHaveAttribute("tabindex", "-1");
  });

  it("관리 탭은 기존 라우트를 그대로 가리킨다 — 리다이렉트하지 않는다", () => {
    renderView();
    expect(screen.getByRole("tab", { name: /Deployments/ })).toHaveAttribute("href", "/deployments?cluster=prod-seoul");
    expect(screen.getByRole("tab", { name: /Secrets/ })).toHaveAttribute("href", "/secrets?cluster=prod-seoul");
  });

  it("좌우 화살표와 Home/End로 탭 사이를 이동한다", () => {
    renderView();
    const [explorer, deployments, secrets] = screen.getAllByRole("tab");
    explorer.focus();
    fireEvent.keyDown(explorer, { key: "ArrowRight" });
    expect(document.activeElement).toBe(deployments);
    fireEvent.keyDown(deployments, { key: "End" });
    expect(document.activeElement).toBe(secrets);
    fireEvent.keyDown(secrets, { key: "Home" });
    expect(document.activeElement).toBe(explorer);
    fireEvent.keyDown(explorer, { key: "ArrowLeft" });
    expect(document.activeElement).toBe(secrets);
  });

  it("Explorer 탭 패널이 탭과 연결되어 있다", () => {
    renderView();
    const panel = screen.getByRole("tabpanel");
    expect(panel).toHaveAttribute("aria-labelledby", "resources-tab-explorer");
    expect(screen.getByRole("tab", { name: "Explorer" })).toHaveAttribute("aria-controls", panel.id);
  });

  it("관리 권한이 없으면 관리 탭을 만들지 않는다", () => {
    renderView({ ...SCOPE, canManageWorkloads: false });
    expect(screen.getAllByRole("tab")).toHaveLength(1);
    expect(screen.queryByRole("tab", { name: /Deployments/ })).toBeNull();
  });

  it("탐색 권한이 없으면 빈 화면이 아니라 권한 안내를 보여준다", () => {
    renderView({ ...SCOPE, canExploreResources: false });
    const alert = screen.getByRole("alert");
    expect(alert.textContent ?? "").toContain("리소스 탐색 권한이 없습니다");
    expect(alert.textContent ?? "").toContain("조회가 거절");
  });
});
