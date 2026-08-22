import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import type { ReactNode } from "react";
import { MemoryRouter, useLocation } from "react-router-dom";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type { ResourceSearchResponse } from "@k8s-dashboard/contracts";
import { HttpError } from "@/api/client";

afterEach(cleanup);

/* 레지스트리는 진짜를 씁니다 — 화면 구현만 가볍게 바꿉니다. */
vi.mock("@/features/overview/ClusterOverview", () => ({ ClusterOverview: () => null }));
vi.mock("@/features/nodes/NodesView", () => ({ NodesView: () => null }));
vi.mock("@/features/drill/NamespaceList", () => ({ NamespaceList: () => null }));
vi.mock("@/features/drill/NamespaceDetail", () => ({ NamespaceDetail: () => null }));
vi.mock("@/features/drill/WorkloadDetail", () => ({ WorkloadDetail: () => null }));
vi.mock("@/features/drill/PodDetail", () => ({ PodDetail: () => null }));
vi.mock("@/features/logs/LogsExplorer", () => ({ LogsExplorer: () => null }));
vi.mock("@/features/topology/TopologyView", () => ({ TopologyView: () => null }));
vi.mock("@/features/alerts/AlertsView", () => ({ AlertsView: () => null }));
vi.mock("@/features/resources/ResourcesView", () => ({ ResourcesView: () => null }));
vi.mock("@/features/dashboards/DashboardView", () => ({ DashboardView: () => null }));
vi.mock("@/features/manage/ManageView", () => ({ ManageView: () => null }));
vi.mock("@/features/dashboard-builder/DashboardBuilder", () => ({
  DashboardBuilderList: () => null,
  DashboardBuilderEditor: () => null,
}));

const hooks = vi.hoisted(() => ({ search: vi.fn(), recent: vi.fn() }));

vi.mock("@/api/queries", async () => {
  const actual = await vi.importActual<typeof import("@/api/queries")>("@/api/queries");
  return {
    ...actual,
    useResourceSearch: (...args: unknown[]) => hooks.search(...args),
    useRecentResources: (...args: unknown[]) => hooks.recent(...args),
  };
});

import { CommandPalette } from "./CommandPalette";

const ITEM = {
  group: "core",
  version: "v1",
  resource: "services",
  kind: "Service",
  namespaced: true,
  namespace: "payments",
  name: "payments-api",
  uid: "uid-1",
  matchedField: "name" as const,
};

function response(over: Partial<ResourceSearchResponse> = {}): ResourceSearchResponse {
  return {
    clusterId: "prod-seoul",
    query: "pay",
    generatedAt: "2026-08-13T04:00:00Z",
    observedAt: "2026-08-13T04:00:00Z",
    appliedScope: { clusterId: "prod-seoul", namespaces: "all" },
    items: [ITEM],
    truncated: false,
    degraded: false,
    ...over,
  };
}

function stubSearch(over: Record<string, unknown> = {}) {
  hooks.search.mockReturnValue({
    data: undefined,
    error: undefined,
    isLoading: false,
    isFetching: false,
    isSuccess: false,
    ...over,
  });
}

function stubRecent(over: Record<string, unknown> = {}) {
  hooks.recent.mockReturnValue({
    data: undefined,
    error: undefined,
    isLoading: false,
    isFetching: false,
    isSuccess: false,
    ...over,
  });
}

let lastLocation = "";
function LocationProbe() {
  const loc = useLocation();
  lastLocation = `${loc.pathname}${loc.search}`;
  return null;
}

function renderPalette(
  caps = { canExploreResources: true, canManageWorkloads: true },
  extra?: ReactNode,
) {
  /* 데이터 훅은 mock이지만 팔레트가 취소를 위해 QueryClient를 봅니다. */
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={client}>
      <MemoryRouter initialEntries={["/?cluster=prod-seoul"]}>
        <LocationProbe />
        <button type="button">이전 포커스</button>
        {extra}
        <CommandPalette clusterId="prod-seoul" caps={caps} navSearch="?cluster=prod-seoul" />
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

/** Cmd/Ctrl+K는 window 리스너입니다 — 실제 사용자 경로와 같게 window에 보냅니다. */
function openPalette() {
  act(() => {
    fireEvent.keyDown(window, { key: "k", ctrlKey: true });
  });
}

/** 디바운스(200ms)를 지나 요청 상태로 넘깁니다. */
function settleDebounce() {
  act(() => {
    vi.advanceTimersByTime(250);
  });
}

beforeEach(() => {
  vi.useFakeTimers();
  stubSearch();
  stubRecent();
  window.localStorage.clear();
});

afterEach(() => {
  vi.useRealTimers();
});

describe("열기·닫기와 포커스", () => {
  it("Ctrl+K로 열리고 입력에 포커스가 갑니다", () => {
    renderPalette();
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
    openPalette();
    act(() => {
      vi.advanceTimersByTime(10);
    });
    expect(screen.getByRole("dialog", { name: "명령 팔레트" })).toBeInTheDocument();
    expect(screen.getByRole("combobox")).toHaveFocus();
  });

  it("Esc로 닫고 열기 전 포커스를 되돌립니다", () => {
    renderPalette();
    const before = screen.getByRole("button", { name: "이전 포커스" });
    before.focus();
    openPalette();
    act(() => {
      vi.advanceTimersByTime(10);
    });
    fireEvent.keyDown(screen.getByRole("combobox"), { key: "Escape" });
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
    expect(before).toHaveFocus();
  });

  it("IME 조합 중에는 열지 않습니다", () => {
    renderPalette();
    act(() => {
      fireEvent.keyDown(window, { key: "k", ctrlKey: true, isComposing: true });
    });
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
  });

  it("Ctrl/Cmd+K는 토글입니다 — 두 번 누르면 닫힙니다", () => {
    renderPalette();
    openPalette();
    expect(screen.getByRole("dialog", { name: "명령 팔레트" })).toBeInTheDocument();
    act(() => {
      fireEvent.keyDown(window, { key: "K", metaKey: true });
    });
    expect(screen.queryByRole("dialog", { name: "명령 팔레트" })).not.toBeInTheDocument();
  });

  it("이미 dialog가 열려 있으면 그 위에 겹치지 않습니다", () => {
    renderPalette(
      { canExploreResources: true, canManageWorkloads: true },
      <div role="dialog" aria-modal="true" aria-label="리소스 상세">
        <button type="button">닫기</button>
      </div>,
    );
    openPalette();
    expect(screen.queryByRole("dialog", { name: "명령 팔레트" })).not.toBeInTheDocument();
  });

  it("확인 창(alertdialog) 위에도 겹치지 않습니다", () => {
    /* ConfirmDialog는 role="alertdialog"입니다. 이걸 빠뜨리면 파괴적 동작을
       확인받는 창 위에 팔레트가 덮여, 사용자가 무엇에 답하는지 모르게 됩니다. */
    renderPalette(
      { canExploreResources: true, canManageWorkloads: true },
      <div role="alertdialog" aria-modal="true" aria-label="삭제 확인">
        <button type="button">확인</button>
      </div>,
    );
    openPalette();
    expect(screen.queryByRole("dialog", { name: "명령 팔레트" })).not.toBeInTheDocument();
  });

  it("Tab·Shift+Tab이 다이얼로그 밖으로 빠져나가지 않습니다", () => {
    renderPalette();
    openPalette();
    act(() => {
      vi.advanceTimersByTime(10);
    });
    const dialog = screen.getByRole("dialog", { name: "명령 팔레트" });
    const input = screen.getByRole("combobox");

    const forward = fireEvent.keyDown(input, { key: "Tab" });
    expect(forward).toBe(false); // preventDefault — 브라우저 기본 이동을 막습니다
    expect(dialog.contains(document.activeElement)).toBe(true);

    const back = fireEvent.keyDown(input, { key: "Tab", shiftKey: true });
    expect(back).toBe(false);
    expect(dialog.contains(document.activeElement)).toBe(true);
    /* 밖의 버튼으로는 절대 가지 않습니다. */
    expect(screen.getByRole("button", { name: "이전 포커스" })).not.toHaveFocus();
  });

  it("포커스가 밖으로 나가 있어도 Esc가 닫습니다", () => {
    renderPalette();
    const before = screen.getByRole("button", { name: "이전 포커스" });
    before.focus();
    openPalette();
    act(() => {
      vi.advanceTimersByTime(10);
    });
    /* 마우스나 보조기술로 포커스가 배경으로 옮겨간 상황입니다. */
    before.focus();
    fireEvent.keyDown(document.body, { key: "Escape" });
    expect(screen.queryByRole("dialog", { name: "명령 팔레트" })).not.toBeInTheDocument();
    expect(before).toHaveFocus();
  });

  it("IME 조합 중 Enter는 행을 실행하지 않습니다", () => {
    renderPalette();
    openPalette();
    const input = screen.getByRole("combobox");
    /* 첫 항목이 아닌 곳으로 옮겨 두어야 "이동하지 않았음"이 관측됩니다. */
    fireEvent.keyDown(input, { key: "ArrowDown" });
    fireEvent.keyDown(input, { key: "Enter", isComposing: true });
    expect(screen.getByRole("dialog", { name: "명령 팔레트" })).toBeInTheDocument();
    expect(lastLocation).toBe("/?cluster=prod-seoul");
  });
});

describe("ARIA 구조", () => {
  it("combobox·listbox·option과 aria-activedescendant가 이어집니다", () => {
    renderPalette();
    openPalette();
    const input = screen.getByRole("combobox");
    const list = screen.getByRole("listbox", { name: "결과" });
    expect(input).toHaveAttribute("aria-expanded", "true");
    expect(input).toHaveAttribute("aria-controls", list.id);

    const options = within(list).getAllByRole("option");
    expect(options.length).toBeGreaterThan(0);
    expect(input.getAttribute("aria-activedescendant")).toBe(options[0].id);
    expect(options[0]).toHaveAttribute("aria-selected", "true");
    expect(options[1]).toHaveAttribute("aria-selected", "false");
  });

  it("결과 수를 live 영역으로 알립니다", () => {
    renderPalette();
    openPalette();
    const status = screen.getByRole("status");
    expect(status).toHaveAttribute("aria-live", "polite");
    expect(status.textContent).toMatch(/건의 결과$/);
  });

  it("입력 상한이 서버 상한과 같습니다", () => {
    renderPalette();
    openPalette();
    expect(screen.getByRole("combobox")).toHaveAttribute("maxlength", "64");
  });
});

describe("키보드 이동", () => {
  it("ArrowDown/ArrowUp이 순환하고 Enter가 선택을 엽니다", () => {
    renderPalette();
    openPalette();
    const input = screen.getByRole("combobox");
    const options = within(screen.getByRole("listbox")).getAllByRole("option");

    fireEvent.keyDown(input, { key: "ArrowDown" });
    expect(input.getAttribute("aria-activedescendant")).toBe(options[1].id);
    fireEvent.keyDown(input, { key: "ArrowUp" });
    expect(input.getAttribute("aria-activedescendant")).toBe(options[0].id);
    /* 첫 항목에서 위로 가면 마지막으로 돕니다 — 막다른 끝을 만들지 않습니다. */
    fireEvent.keyDown(input, { key: "ArrowUp" });
    expect(input.getAttribute("aria-activedescendant")).toBe(options[options.length - 1].id);

    fireEvent.keyDown(input, { key: "Home" });
    fireEvent.keyDown(input, { key: "Enter" });
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
    /* 첫 항목은 레지스트리의 첫 이동 목적지(Cluster Overview)입니다. */
    expect(lastLocation).toBe("/?cluster=prod-seoul");
  });
});

describe("이동 그룹", () => {
  it("capability로 걸러진 레지스트리 항목만 나옵니다", () => {
    renderPalette({ canExploreResources: false, canManageWorkloads: false });
    openPalette();
    const list = screen.getByRole("listbox");
    expect(within(list).queryByText("Deployments")).not.toBeInTheDocument();
    expect(within(list).queryByText("Resources")).not.toBeInTheDocument();
    expect(within(list).getByText("Cluster Overview")).toBeInTheDocument();
  });

  it("입력으로 목적지를 좁힐 수 있습니다", () => {
    renderPalette();
    openPalette();
    fireEvent.change(screen.getByRole("combobox"), { target: { value: "topo" } });
    const list = screen.getByRole("listbox");
    expect(within(list).getByText("Pod Topology")).toBeInTheDocument();
    expect(within(list).queryByText("Cluster Overview")).not.toBeInTheDocument();
  });
});

describe("리소스 검색", () => {
  it("2자 미만이면 요청하지 않고 이유를 말합니다", () => {
    renderPalette();
    openPalette();
    fireEvent.change(screen.getByRole("combobox"), { target: { value: "p" } });
    settleDebounce();
    expect(screen.getByText(/2자 이상 입력해야/)).toBeInTheDocument();
    /* enabled가 false로 전달되어 훅이 요청을 만들지 않습니다. */
    expect(hooks.search).toHaveBeenCalledWith("prod-seoul", "p", false);
  });

  it("2자 이상이면 디바운스 뒤 한 번만 질의합니다", () => {
    renderPalette();
    openPalette();
    const input = screen.getByRole("combobox");
    fireEvent.change(input, { target: { value: "pa" } });
    fireEvent.change(input, { target: { value: "pay" } });
    settleDebounce();
    /* 마지막 값만 요청됩니다 — 중간 값으로는 enabled가 켜지지 않습니다. */
    expect(hooks.search).toHaveBeenCalledWith("prod-seoul", "pay", true);
    expect(hooks.search).not.toHaveBeenCalledWith("prod-seoul", "pa", true);
  });

  it("kind·이름·namespace를 보여주고 상태는 만들지 않습니다", () => {
    stubSearch({ data: response(), isSuccess: true });
    renderPalette();
    openPalette();
    fireEvent.change(screen.getByRole("combobox"), { target: { value: "pay" } });
    settleDebounce();
    const option = screen.getByRole("option", { name: /payments-api/ });
    expect(option).toHaveTextContent("Service");
    expect(option).toHaveTextContent("payments");
    expect(option).not.toHaveTextContent(/Running|Ready|Healthy/);
  });

  it("선택하면 GVR·namespace·이름·UID가 담긴 deep link로 이동합니다", () => {
    stubSearch({ data: response(), isSuccess: true });
    renderPalette();
    openPalette();
    fireEvent.change(screen.getByRole("combobox"), { target: { value: "pay" } });
    settleDebounce();
    fireEvent.mouseDown(screen.getByRole("option", { name: /payments-api/ }));

    expect(lastLocation.startsWith("/resources?")).toBe(true);
    const params = new URLSearchParams(lastLocation.split("?")[1]);
    expect(params.get("cluster")).toBe("prod-seoul");
    expect(params.get("res")).toBe("core/v1/services");
    expect(params.get("ns")).toBe("payments");
    expect(params.get("item")).toBe("payments/payments-api/uid-1");
  });

  it("선택한 항목이 최근 목록에 남습니다", () => {
    stubSearch({ data: response(), isSuccess: true });
    renderPalette();
    openPalette();
    fireEvent.change(screen.getByRole("combobox"), { target: { value: "pay" } });
    settleDebounce();
    fireEvent.mouseDown(screen.getByRole("option", { name: /payments-api/ }));
    const stored = JSON.parse(window.localStorage.getItem("k8s-dashboard.recent.v1") ?? "{}");
    expect(stored.v).toBe(1);
    /* 클러스터가 신원의 일부입니다 — 없으면 다른 클러스터의 같은 UID와 섞입니다. */
    expect(stored.items[0]).toMatchObject({
      clusterId: "prod-seoul",
      uid: "uid-1",
      name: "payments-api",
      resource: "services",
    });
  });
});

describe("상태 구분", () => {
  it("검색 기능이 없는 배포는 이동만 남기고 이유를 알립니다", () => {
    renderPalette({ canExploreResources: false, canManageWorkloads: true });
    openPalette();
    expect(screen.getByText(/리소스 검색을 사용할 수 없습니다/)).toBeInTheDocument();
    expect(screen.getByRole("listbox")).toBeInTheDocument();
  });

  it("동기화 중·빈 결과·오류를 서로 다른 문구로 구분합니다", () => {
    stubSearch({
      error: new HttpError(503, { code: "resource_syncing", message: "…", requestId: "r" }),
    });
    renderPalette();
    openPalette();
    fireEvent.change(screen.getByRole("combobox"), { target: { value: "pay" } });
    settleDebounce();
    expect(screen.getByText(/동기화하는 중/)).toBeInTheDocument();
    cleanup();

    stubSearch({ data: response({ items: [] }), isSuccess: true });
    renderPalette();
    openPalette();
    fireEvent.change(screen.getByRole("combobox"), { target: { value: "zzz" } });
    settleDebounce();
    expect(screen.getByText(/일치하는 리소스가 없습니다/)).toBeInTheDocument();
    cleanup();

    stubSearch({ error: new HttpError(502, { code: "upstream_unavailable", message: "boom", requestId: "r" }) });
    renderPalette();
    openPalette();
    fireEvent.change(screen.getByRole("combobox"), { target: { value: "pay" } });
    settleDebounce();
    expect(screen.getByRole("alert")).toHaveTextContent(/검색하지 못했습니다/);
  });

  it("부분 색인은 사유와 함께 알립니다 — 완전한 검색처럼 보이지 않습니다", () => {
    stubSearch({ data: response({ degraded: true, reason: "label 예산 초과" }), isSuccess: true });
    renderPalette();
    openPalette();
    fireEvent.change(screen.getByRole("combobox"), { target: { value: "pay" } });
    settleDebounce();
    expect(screen.getByText(/색인이 완전하지 않습니다/)).toHaveTextContent("label 예산 초과");
  });

  it("상한에 걸린 결과는 잘렸다고 명시합니다", () => {
    stubSearch({ data: response({ truncated: true }), isSuccess: true });
    renderPalette();
    openPalette();
    fireEvent.change(screen.getByRole("combobox"), { target: { value: "pay" } });
    settleDebounce();
    expect(screen.getByText(/상위 50건만 표시했습니다/)).toBeInTheDocument();
  });

  it("부분 색인은 사유가 없어도 알립니다 — 선택 필드라고 침묵하지 않습니다", () => {
    stubSearch({ data: response({ degraded: true }), isSuccess: true });
    renderPalette();
    openPalette();
    fireEvent.change(screen.getByRole("combobox"), { target: { value: "pay" } });
    settleDebounce();
    expect(screen.getByText(/색인이 완전하지 않습니다/)).toBeInTheDocument();
  });
});

describe("디바운스 중 신선도", () => {
  it("입력이 앞서면 낡은 결과를 지우고 '찾는 중'으로 답합니다", () => {
    stubSearch({ data: response(), isSuccess: true });
    renderPalette();
    openPalette();
    const input = screen.getByRole("combobox");
    fireEvent.change(input, { target: { value: "pay" } });
    settleDebounce();
    expect(screen.getByRole("option", { name: /payments-api/ })).toBeInTheDocument();

    /* 여기서 손에 든 값은 "pay"의 결과이고 화면의 입력은 "payments"입니다. */
    fireEvent.change(input, { target: { value: "payments" } });
    expect(screen.queryByRole("option", { name: /payments-api/ })).not.toBeInTheDocument();
    expect(screen.getByText("찾는 중…")).toBeInTheDocument();
    /* 실행할 행 자체가 없으므로 Enter로도 열리지 않습니다. */
    fireEvent.keyDown(input, { key: "Enter" });
    expect(lastLocation).toBe("/?cluster=prod-seoul");
  });

  it("0·1자로 지우면 즉시 idle·short이고 옛 리소스 행이 남지 않습니다", () => {
    stubSearch({ data: response(), isSuccess: true });
    renderPalette();
    openPalette();
    const input = screen.getByRole("combobox");
    fireEvent.change(input, { target: { value: "pay" } });
    settleDebounce();
    expect(screen.getByRole("option", { name: /payments-api/ })).toBeInTheDocument();

    fireEvent.change(input, { target: { value: "p" } });
    expect(screen.queryByRole("option", { name: /payments-api/ })).not.toBeInTheDocument();
    expect(screen.getByText(/2자 이상 입력해야/)).toBeInTheDocument();

    fireEvent.change(input, { target: { value: "" } });
    expect(screen.queryByRole("option", { name: /payments-api/ })).not.toBeInTheDocument();
    expect(screen.getByText("최근 항목")).toBeInTheDocument();
  });

  it("앞뒤 공백만 다른 입력은 같은 질의로 봅니다", () => {
    stubSearch({ data: response(), isSuccess: true });
    renderPalette();
    openPalette();
    const input = screen.getByRole("combobox");
    fireEvent.change(input, { target: { value: "pay" } });
    settleDebounce();
    fireEvent.change(input, { target: { value: " pay " } });
    /* 정규화가 한쪽에만 걸리면 여기서 결과가 사라집니다. */
    expect(screen.getByRole("option", { name: /payments-api/ })).toBeInTheDocument();
  });
});

describe("최근 항목", () => {
  it("재확인이 도는 동안에는 캐시된 제목을 보여주지 않습니다", () => {
    stubRecent({ data: [{ ...ITEM }], isSuccess: true, isFetching: true });
    renderPalette();
    openPalette();
    expect(screen.queryByRole("option", { name: /payments-api/ })).not.toBeInTheDocument();
    expect(screen.getByText(/다시 확인하는 중/)).toBeInTheDocument();
  });

  it("불러오지 못하면 빈 목록으로 접지 않고 이유를 말합니다", () => {
    stubRecent({ error: new HttpError(502, { code: "upstream_unavailable", message: "boom", requestId: "r" }) });
    renderPalette();
    openPalette();
    expect(screen.getByRole("alert")).toHaveTextContent(/최근 항목을 불러오지 못했습니다/);
  });

  it("검색이 꺼진 배포는 최근 항목도 사용할 수 없다고 말합니다", () => {
    stubRecent({ error: new HttpError(503, { code: "resources_unavailable", message: "off", requestId: "r" }) });
    renderPalette();
    openPalette();
    expect(screen.getByText(/리소스 검색을 사용할 수 없습니다/)).toBeInTheDocument();
  });

  it("정말 비어 있으면 비었다고 말합니다", () => {
    stubRecent({ data: [], isSuccess: true });
    renderPalette();
    openPalette();
    expect(screen.getByText(/최근 본 리소스가 없습니다/)).toBeInTheDocument();
  });
});
