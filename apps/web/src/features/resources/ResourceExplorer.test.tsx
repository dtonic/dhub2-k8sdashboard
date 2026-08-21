import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type { ResourceDescriptor, ResourceListResponse, ScopeResponse } from "@k8s-dashboard/contracts";
import { HttpError } from "@/api/client";

/* 이 프로젝트 vitest 설정은 globals가 없어 RTL 자동 cleanup이 걸리지 않습니다. */
afterEach(cleanup);

const hooks = vi.hoisted(() => ({
  scope: vi.fn(),
  catalog: vi.fn(),
  list: vi.fn(),
  object: vi.fn(),
}));

vi.mock("@/api/queries", () => ({
  useScope: () => hooks.scope(),
  useResourceCatalog: () => hooks.catalog(),
  useResourceList: (...args: unknown[]) => hooks.list(...args),
  useResourceObject: (...args: unknown[]) => hooks.object(...args),
}));

import { ResourceExplorer } from "./ResourceExplorer";

const SCOPE: ScopeResponse = {
  clusters: [{ id: "prod-seoul", name: "prod-seoul", namespaces: "all", accessible: true, availableNamespaces: ["payments", "search"] }],
  canManageWorkloads: true,
  canExploreResources: true,
};

const SERVICES: ResourceDescriptor = {
  group: "core", version: "v1", resource: "services", kind: "Service",
  namespaced: true, verbs: ["get", "list", "watch"], state: "ready", count: 2,
};

function page(over: Partial<ResourceListResponse> = {}): ResourceListResponse {
  return {
    clusterId: "prod-seoul", group: "core", version: "v1", resource: "services",
    kind: "Service", namespaced: true, generatedAt: "2026-08-13T04:00:00Z",
    observedAt: "2026-08-13T04:00:00Z",
    appliedScope: { clusterId: "prod-seoul", namespaces: "all" },
    items: [
      { namespace: "payments", name: "payments-api", uid: "uid-1", createdAt: "2026-08-10T00:00:00Z" },
      { namespace: "search", name: "indexer", uid: "uid-2", createdAt: "2026-08-11T00:00:00Z" },
    ],
    truncated: false,
    total: 2,
    ...over,
  };
}

type ListStub = {
  pages?: ResourceListResponse[];
  error?: unknown;
  hasNextPage?: boolean;
  isLoading?: boolean;
  isSuccess?: boolean;
};

const fetchNextPage = vi.fn();
const refetch = vi.fn();

function stubList(stub: ListStub = {}) {
  hooks.list.mockReturnValue({
    data: stub.pages ? { pages: stub.pages } : undefined,
    error: stub.error,
    isLoading: stub.isLoading ?? false,
    isSuccess: stub.isSuccess ?? Boolean(stub.pages),
    isFetching: false,
    isFetchingNextPage: false,
    hasNextPage: stub.hasNextPage ?? false,
    fetchNextPage,
    refetch,
  });
}

function renderExplorer(path = "/resources?cluster=prod-seoul&res=core/v1/services") {
  return render(
    <MemoryRouter initialEntries={[path]}>
      <ResourceExplorer />
    </MemoryRouter>,
  );
}

beforeEach(() => {
  fetchNextPage.mockReset();
  refetch.mockReset();
  hooks.scope.mockReturnValue({ data: SCOPE });
  hooks.catalog.mockReturnValue({ data: { clusterId: "prod-seoul", generatedAt: "", degraded: false, items: [SERVICES] }, isLoading: false, error: undefined });
  hooks.object.mockReturnValue({ data: undefined, isLoading: false, error: undefined });
  stubList({ pages: [page()] });
});

describe("ResourceExplorer", () => {
  it("BFF 목록 결과를 이름·Namespace와 함께 그린다", () => {
    renderExplorer();
    const table = screen.getByRole("table");
    expect(within(table).getByRole("button", { name: "payments-api" })).toBeVisible();
    expect(within(table).getByText("search")).toBeVisible();
  });

  it("생성·수정·삭제 컨트롤이 없다 — 조회 전용 화면이다", () => {
    renderExplorer();
    const forbidden = /삭제|생성|수정|저장|배포|추가|편집/;
    for (const el of screen.getAllByRole("button")) {
      expect(el.textContent ?? "").not.toMatch(forbidden);
    }
    expect(screen.queryByRole("textbox", { name: /매니페스트/ })).toBeNull();
  });

  it("이름·라벨 필터는 서버 상한으로 묶여 있다", () => {
    renderExplorer();
    expect(screen.getByLabelText("이름 prefix")).toHaveAttribute("maxlength", "253");
    expect(screen.getByLabelText("Label selector")).toHaveAttribute("maxlength", "512");
  });

  it("필터는 타이핑마다가 아니라 조회할 때 요청에 반영된다", () => {
    renderExplorer();
    const before = hooks.list.mock.calls.length;
    fireEvent.change(screen.getByLabelText("이름 prefix"), { target: { value: "payments-" } });
    /* 입력만으로는 새 필터가 요청 인자로 들어가지 않습니다. */
    for (const call of hooks.list.mock.calls.slice(before)) {
      expect((call[0] as { namePrefix: string }).namePrefix).toBe("");
    }
    fireEvent.click(screen.getByRole("button", { name: "조회" }));
    const last = hooks.list.mock.calls.at(-1)?.[0] as { namePrefix: string };
    expect(last.namePrefix).toBe("payments-");
  });

  it("서버 cursor가 있을 때만 더 보기를 노출하고 offset을 만들지 않는다", () => {
    stubList({ pages: [page({ nextCursor: "b3BhcXVl", truncated: true })], hasNextPage: true });
    renderExplorer();
    fireEvent.click(screen.getByRole("button", { name: "더 보기" }));
    expect(fetchNextPage).toHaveBeenCalledTimes(1);

    cleanup();
    stubList({ pages: [page()] });
    renderExplorer();
    expect(screen.queryByRole("button", { name: "더 보기" })).toBeNull();
    expect(screen.getByText(/마지막 페이지/)).toBeVisible();
  });

  it.each([
    ["syncing", "동기화"],
    ["unsupported", "metadata 전용"],
    ["forbidden", "권한"],
    ["missing", "제공하지 않습니다"],
  ] as const)("카탈로그 상태 %s를 고유한 안내로 구분한다", (state, text) => {
    hooks.catalog.mockReturnValue({
      data: { clusterId: "prod-seoul", generatedAt: "", degraded: false, items: [{ ...SERVICES, state }] },
      isLoading: false,
      error: undefined,
    });
    stubList({ pages: [page({ items: [], total: 0 })] });
    renderExplorer();
    const notice = document.querySelector(`[data-state="${state}"]`);
    expect(notice).not.toBeNull();
    expect(notice?.textContent ?? "").toContain(text);
    expect(screen.queryByRole("table")).toBeNull();
  });

  it("0건은 동기화 중과 다른 화면이다", () => {
    stubList({ pages: [page({ items: [], total: 0 })] });
    renderExplorer();
    expect(document.querySelector('[data-state="empty"]')).not.toBeNull();
    expect(document.querySelector('[data-state="syncing"]')).toBeNull();
  });

  it("allowlist에서 빠진 GVR deep link는 빈 표가 아니라 미등록을 알린다", () => {
    hooks.catalog.mockReturnValue({
      data: { clusterId: "prod-seoul", generatedAt: "", degraded: false, items: [] },
      isLoading: false,
      error: undefined,
    });
    renderExplorer();
    expect(document.querySelector('[data-state="missing"]')).not.toBeNull();
    expect(screen.queryByRole("table")).toBeNull();
    /* 카탈로그에 없는 GVR로는 목록 요청 자체를 켜지 않습니다. */
    expect(hooks.list.mock.calls.at(-1)?.[1]).toBe(false);
  });

  it("기능이 없는 배포는 필터도 그리지 않고 unavailable만 알린다", () => {
    hooks.scope.mockReturnValue({ data: { ...SCOPE, canExploreResources: false } });
    renderExplorer();
    expect(document.querySelector('[data-state="unavailable"]')).not.toBeNull();
    expect(screen.queryByLabelText("이름 prefix")).toBeNull();
  });

  it("목록 400은 상태가 아니라 요청 오류로 알린다", () => {
    stubList({ error: new HttpError(400, { code: "invalid_filter", message: "", requestId: "r" }), isSuccess: false });
    renderExplorer();
    expect(screen.getByRole("alert").textContent ?? "").toContain("필터 값이 서버 상한");
  });

  it("리소스를 고르기 전에는 목록 요청을 켜지 않는다", () => {
    renderExplorer("/resources?cluster=prod-seoul");
    expect(hooks.list.mock.calls.at(-1)?.[1]).toBe(false);
    expect(screen.getByText("리소스 종류를 선택하세요")).toBeVisible();
  });
});
