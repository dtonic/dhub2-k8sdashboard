import { cleanup, render, screen } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { afterEach, describe, expect, it, vi } from "vitest";
import type { ScopeResponse } from "@k8s-dashboard/contracts";

afterEach(cleanup);

const hooks = vi.hoisted(() => ({ scope: vi.fn() }));

vi.mock("@/api/queries", () => ({ useScope: () => hooks.scope() }));
vi.mock("@/app/AuthGate", () => ({ useAuth: () => ({ enabled: false, session: undefined, logout: vi.fn() }) }));
vi.mock("@/app/StreamInvalidator", () => ({ StreamInvalidator: () => null }));
vi.mock("@/generated/dashboards", () => ({ embeddedDashboards: [] }));
vi.mock("@/lib/env", () => ({ usingMockApi: false }));

import { AppShell } from "./AppShell";

const BASE: ScopeResponse = {
  clusters: [{ id: "prod-seoul", name: "prod-seoul", namespaces: "all", accessible: true }],
};

function renderShell(scope: ScopeResponse) {
  hooks.scope.mockReturnValue({ data: scope, isError: false });
  render(
    <MemoryRouter initialEntries={["/?cluster=prod-seoul"]}>
      <Routes>
        <Route path="/" element={<AppShell />} />
      </Routes>
    </MemoryRouter>,
  );
}

const navLink = (name: string) => screen.queryByRole("link", { name });

describe("AppShell 내비게이션 보존 (ADR 0018)", () => {
  it("관측 화면 링크는 그대로 있다", () => {
    renderShell(BASE);
    for (const label of ["Cluster Overview", "Nodes", "Namespaces", "Pod Topology", "Logs Explorer", "Alerts"]) {
      expect(navLink(label), `${label} 링크가 사라졌습니다`).not.toBeNull();
    }
  });

  it("관리 그룹 노출 조건은 canManageWorkloads 그대로다", () => {
    renderShell({ ...BASE, canManageWorkloads: true });
    expect(navLink("Deployments")).toHaveAttribute("href", "/deployments?cluster=prod-seoul");
    expect(navLink("Secrets")).toHaveAttribute("href", "/secrets?cluster=prod-seoul");

    cleanup();
    renderShell({ ...BASE, canManageWorkloads: false });
    expect(navLink("Deployments")).toBeNull();
    expect(navLink("Secrets")).toBeNull();
  });

  it("Resources 항목은 canExploreResources로만 열린다", () => {
    renderShell({ ...BASE, canExploreResources: true });
    expect(navLink("Resources")).toHaveAttribute("href", "/resources?cluster=prod-seoul");

    cleanup();
    renderShell({ ...BASE, canExploreResources: false });
    expect(navLink("Resources")).toBeNull();

    cleanup();
    renderShell(BASE);
    expect(navLink("Resources")).toBeNull();
  });

  it("탐색 권한과 관리 권한은 서로를 켜지 않는다", () => {
    renderShell({ ...BASE, canExploreResources: true, canManageWorkloads: false });
    expect(navLink("Resources")).not.toBeNull();
    expect(navLink("Deployments")).toBeNull();

    cleanup();
    renderShell({ ...BASE, canExploreResources: false, canManageWorkloads: true });
    expect(navLink("Resources")).toBeNull();
    expect(navLink("Deployments")).not.toBeNull();
  });
});
