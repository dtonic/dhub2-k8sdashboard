import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter, useLocation } from "react-router-dom";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { encodeRecentRef, MAX_RECENT, MAX_RECENT_REQUEST_TARGET_BYTES, type RecentRef } from "./recent";

/**
 * 실제 QueryClient 위에서의 취소·요청 크기 (ADR 0023)
 * --------------------------------------------------------------------------
 * 다른 팔레트 테스트는 데이터 훅을 mock하지만, **취소는 mock으로 증명되지
 * 않습니다.** `enabled: false`는 QueryObserver를 마운트된 채로 두고 이미 나간
 * fetch는 계속 달립니다. 그래서 여기서는 진짜 QueryClient와 지연 가능한 fetch를
 * 놓고 `AbortSignal.aborted`를 직접 확인합니다.
 *
 * 요청 크기도 같습니다 — ref 조각만 재면 경로가 긴 배포에서만 414가 납니다.
 * 여기서는 브라우저가 실제로 만든 URL을 붙잡아 `pathname + search`의 UTF-8
 * 바이트를 잽니다.
 */

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

import { CommandPalette } from "./CommandPalette";

const STORAGE_KEY = "k8s-dashboard.recent.v1";
const ISO = "2026-08-13T04:00:00Z";
const SCOPE = { clusterId: "prod-seoul", namespaces: "all" as const };

/* ── 지연 가능한 fetch ─────────────────────────────────────────────────── */

interface Pending {
  url: URL;
  signal: AbortSignal;
  settled: boolean;
  settle(body: unknown): void;
  fail(status: number, body: unknown): void;
}

let pending: Pending[] = [];

function fakeResponse(status: number, body: unknown): Response {
  return {
    ok: status < 400,
    status,
    headers: new Headers(),
    json: async () => body,
  } as unknown as Response;
}

function installFetch() {
  pending = [];
  vi.stubGlobal(
    "fetch",
    vi.fn((input: unknown, init?: RequestInit) => {
      const url = input instanceof URL ? input : new URL(String(input), window.location.origin);
      return new Promise<Response>((resolve) => {
        const call: Pending = {
          url,
          signal: init?.signal as AbortSignal,
          settled: false,
          settle(body) {
            call.settled = true;
            resolve(fakeResponse(200, body));
          },
          fail(status, body) {
            call.settled = true;
            resolve(fakeResponse(status, body));
          },
        };
        pending.push(call);
      });
    }),
  );
}

const isSearch = (u: URL) => u.pathname.endsWith("/resources/search");
const isRecent = (u: URL) => u.pathname.endsWith("/resources/recent");
const recentCalls = () => pending.filter((p) => isRecent(p.url));
const refsOf = (call: Pending) => call.url.searchParams.getAll("ref");
/** 참조가 실린 요청만. 예비 0건 probe와 구분해서 셉니다. */
const chunkCalls = () => recentCalls().filter((p) => refsOf(p).length > 0);
const zeroProbes = () => recentCalls().filter((p) => refsOf(p).length === 0);

/** 아직 관측하지 않은 호출 하나를 기다립니다. */
async function nextCall(match: (u: URL) => boolean, seen: Pending[] = []): Promise<Pending> {
  let found: Pending | undefined;
  await waitFor(() => {
    found = pending.find((p) => match(p.url) && !seen.includes(p));
    expect(found).toBeDefined();
  });
  return found!;
}

/** request target(`pathname + search`)의 UTF-8 바이트. 프록시가 보는 그 값입니다. */
const targetBytes = (u: URL) => new TextEncoder().encode(u.pathname + u.search).byteLength;

/* ── 픽스처 ─────────────────────────────────────────────────────────────── */

function makeRefs(count: number, nameLength = 8, clusterId = "prod-seoul"): RecentRef[] {
  return Array.from({ length: count }, (_, i) => ({
    clusterId,
    group: "core",
    version: "v1",
    resource: "services",
    namespace: "payments",
    name: `${"n".repeat(nameLength)}-${i}`,
    uid: `uid-${i}`,
  }));
}

function seed(items: unknown[]) {
  window.localStorage.setItem(STORAGE_KEY, JSON.stringify({ v: 1, items }));
}

function stored(): RecentRef[] {
  return JSON.parse(window.localStorage.getItem(STORAGE_KEY) ?? '{"items":[]}').items;
}

const recentItem = (r: RecentRef) => ({
  group: r.group,
  version: r.version,
  resource: r.resource,
  kind: "Service",
  namespaced: true,
  namespace: r.namespace,
  name: r.name,
  uid: r.uid,
});

const recentBody = (items: unknown[]) => ({
  clusterId: "prod-seoul",
  generatedAt: ISO,
  appliedScope: SCOPE,
  items,
});

/** 응답의 `query`는 그 요청이 실제로 물어본 질의어여야 합니다 — 픽스처가 계약을 속이지 않습니다. */
const searchBody = (query: string, items: unknown[], over: Record<string, unknown> = {}) => ({
  clusterId: "prod-seoul",
  query,
  generatedAt: ISO,
  observedAt: ISO,
  appliedScope: SCOPE,
  items,
  truncated: false,
  degraded: false,
  ...over,
});

const SERVICE = {
  group: "core",
  version: "v1",
  resource: "services",
  kind: "Service",
  namespaced: true,
  namespace: "payments",
  name: "payments-api",
  uid: "uid-api",
  matchedField: "name" as const,
};

/* ── 렌더 ───────────────────────────────────────────────────────────────── */

let lastLocation = "";
function LocationProbe() {
  const loc = useLocation();
  lastLocation = `${loc.pathname}${loc.search}`;
  return null;
}

function renderPalette(caps = { canExploreResources: true, canManageWorkloads: true }, clusterId = "prod-seoul") {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const view = render(
    <QueryClientProvider client={client}>
      <MemoryRouter initialEntries={["/?cluster=prod-seoul"]}>
        <LocationProbe />
        <CommandPalette clusterId={clusterId} caps={caps} navSearch="?cluster=prod-seoul" />
      </MemoryRouter>
    </QueryClientProvider>,
  );
  /** 클러스터를 살아 있는 화면에서 바꿉니다 — 다시 마운트하지 않습니다. */
  const switchCluster = (next: string) =>
    view.rerender(
      <QueryClientProvider client={client}>
        <MemoryRouter initialEntries={["/?cluster=prod-seoul"]}>
          <LocationProbe />
          <CommandPalette clusterId={next} caps={caps} navSearch="?cluster=prod-seoul" />
        </MemoryRouter>
      </QueryClientProvider>,
    );
  return Object.assign(client, { switchCluster });
}

function openPalette() {
  act(() => {
    fireEvent.keyDown(window, { key: "k", ctrlKey: true });
  });
}

/** 마이크로태스크를 흘려 fetch 체인이 다음 단계로 넘어가게 합니다. */
async function flush() {
  await act(async () => {
    await Promise.resolve();
    await Promise.resolve();
  });
}

beforeEach(() => {
  window.localStorage.clear();
  installFetch();
  lastLocation = "";
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

describe("검색 요청 취소", () => {
  it("입력이 앞서면 진행 중인 그 요청 하나가 즉시 끊깁니다", async () => {
    seed([]);
    renderPalette();
    openPalette();
    const input = await screen.findByRole("combobox");

    fireEvent.change(input, { target: { value: "pay" } });
    const first = await nextCall(isSearch);
    expect(first.signal.aborted).toBe(false);
    expect(first.url.searchParams.get("q")).toBe("pay");

    /* 디바운스를 기다리지 않습니다 — 입력이 앞선 순간 이미 낡은 요청입니다. */
    fireEvent.change(input, { target: { value: "payment" } });
    await waitFor(() => expect(first.signal.aborted).toBe(true));

    /* 끊긴 요청이 그래도 응답을 들고 돌아오는 경우입니다(네트워크는 abort를
       늦게 받을 수 있습니다). 화면의 입력이 "payment"인 동안 그 결과는 행으로
       나타나서도, Enter로 실행되어서도 안 됩니다. */
    await act(async () => {
      first.settle(searchBody("pay", [SERVICE]));
    });
    await flush();
    expect(screen.queryByRole("option", { name: /payments-api/ })).not.toBeInTheDocument();
    fireEvent.keyDown(input, { key: "Enter" });
    expect(lastLocation).toBe("/?cluster=prod-seoul");

    /* 현재 질의를 응답해 테스트를 깔끔히 마칩니다 — 뜬 요청을 남기지 않습니다. */
    const second = await nextCall(isSearch, [first]);
    expect(second.url.searchParams.get("q")).toBe("payment");
    await act(async () => {
      second.settle(searchBody("payment", []));
    });
    expect(await screen.findByText(/일치하는 리소스가 없습니다/)).toBeInTheDocument();
    expect(screen.queryByRole("option", { name: /payments-api/ })).not.toBeInTheDocument();
  });

  it("닫으면 진행 중인 검색 요청이 끊깁니다", async () => {
    seed([]);
    renderPalette();
    openPalette();
    const input = await screen.findByRole("combobox");
    fireEvent.change(input, { target: { value: "pay" } });
    const call = await nextCall(isSearch);

    fireEvent.keyDown(document.body, { key: "Escape" });
    await waitFor(() => expect(call.signal.aborted).toBe(true));
  });

  it("취소는 그 질의 하나만 겨냥합니다 — 다른 화면의 질의는 남습니다", async () => {
    seed([]);
    const client = renderPalette();
    /* 팔레트와 무관한 질의를 캐시에 심어 둡니다. */
    client.setQueryData(["resource-catalog", "prod-seoul"], { items: [] });
    openPalette();
    const input = await screen.findByRole("combobox");
    fireEvent.change(input, { target: { value: "pay" } });
    await nextCall(isSearch);

    fireEvent.keyDown(document.body, { key: "Escape" });
    await flush();
    expect(client.getQueryData(["resource-catalog", "prod-seoul"])).toEqual({ items: [] });
  });

  it("낡은 결과는 디바운스 중에도, 늦게 도착해도 실행되지 않습니다", async () => {
    seed([]);
    renderPalette();
    openPalette();
    const input = await screen.findByRole("combobox");

    fireEvent.change(input, { target: { value: "pay" } });
    const first = await nextCall(isSearch);
    await act(async () => {
      first.settle(searchBody("pay", [SERVICE]));
    });
    expect(await screen.findByRole("option", { name: /payments-api/ })).toBeInTheDocument();

    /* 입력이 앞섰습니다. 이미 **완료된** 요청이라 이제 와서 끊을 대상은 없고
       (진행 중이던 요청이 끊기는 것은 앞의 테스트가 증명합니다), 손에 든 결과가
       이전 입력의 것이라는 사실만으로 즉시 사라지고 Enter로도 실행되지 않습니다. */
    fireEvent.change(input, { target: { value: "payment" } });
    expect(screen.queryByRole("option", { name: /payments-api/ })).not.toBeInTheDocument();
    fireEvent.keyDown(input, { key: "Enter" });
    expect(lastLocation).toBe("/?cluster=prod-seoul");

    /* 새 질의가 빈 결과로 돌아와도 옛 결과가 되살아나지 않습니다. */
    const second = await nextCall(isSearch, [first]);
    expect(second.url.searchParams.get("q")).toBe("payment");
    await act(async () => {
      second.settle(searchBody("payment", []));
    });
    /* Query 상태가 실제로 정착한 뒤에 봅니다 — 동기 상태를 지어내지 않습니다. */
    expect(await screen.findByText(/일치하는 리소스가 없습니다/)).toBeInTheDocument();
    expect(screen.queryByRole("option", { name: /payments-api/ })).not.toBeInTheDocument();
  });
});

describe("최근 항목 요청", () => {
  it("정말 비어 있을 때만, 그리고 정확히 한 번 0건 probe를 보냅니다", async () => {
    seed([]);
    renderPalette();
    openPalette();
    const call = await nextCall(isRecent);
    expect(call.url.pathname).toBe("/api/v1/clusters/prod-seoul/resources/recent");
    expect(refsOf(call)).toHaveLength(0);
    await act(async () => {
      call.settle(recentBody([]));
    });
    await flush();
    /* 저장소를 읽기 전에 한 번, 읽은 뒤에 또 한 번 나가면 안 됩니다. */
    expect(recentCalls()).toHaveLength(1);
  });

  it("저장된 참조가 있으면 예비 0건 probe 없이 곧바로 참조를 싣습니다", async () => {
    const refs = makeRefs(3);
    seed(refs);
    renderPalette();
    openPalette();
    const call = await nextCall(isRecent);
    /* 장벽이 없으면 여기서 refs=[]인 probe가 먼저 잡힙니다. */
    expect(refsOf(call)).toEqual(refs.map(encodeRecentRef));
    await act(async () => {
      call.settle(recentBody(refs.map(recentItem)));
    });
    await flush();
    expect(zeroProbes()).toHaveLength(0);
    expect(recentCalls()).toHaveLength(1);
  });

  it("0건 probe가 실패하면 빈 목록이 아니라 이유를 보여줍니다", async () => {
    seed([]);
    renderPalette();
    openPalette();
    const call = await nextCall(isRecent);
    await act(async () => {
      call.fail(503, { code: "search_unavailable", message: "검색이 꺼져 있습니다", requestId: "r" });
    });
    expect(await screen.findByText(/리소스 검색을 사용할 수 없습니다/)).toBeInTheDocument();
  });

  it("모든 덩어리가 6KiB 안이고, 순서와 20개 상한을 지킵니다", async () => {
    const refs = makeRefs(25, 200);
    seed(refs);
    renderPalette();
    openPalette();

    /* 덩어리 수를 가정하지 않습니다 — 크기 공식이 바뀌면 개수도 바뀝니다.
       실제로 나간 요청을 순서대로 하나씩 응답하며 20개가 다 나갈 때까지 봅니다.
       각 덩어리는 **그 덩어리가 물어본 참조**로 답합니다 — 전부 해석되므로
       정리가 끼어들지 않고, 관측 대상이 청킹 그 자체로 남습니다. */
    const byEncoded = new Map(refs.map((r) => [encodeRecentRef(r), r]));
    const seen: Pending[] = [];
    const sent: string[] = [];
    for (let guard = 0; sent.length < MAX_RECENT; guard++) {
      expect(guard).toBeLessThan(MAX_RECENT); // 무한 루프 방지
      const call = await nextCall(isRecent, seen);
      const refsInCall = refsOf(call);
      /* 예비 0건 probe가 섞이면 여기서 걸립니다. */
      expect(refsInCall.length).toBeGreaterThan(0);
      expect(refsInCall.length).toBeLessThanOrEqual(MAX_RECENT);
      /* 프록시가 보는 request target 전체를 잽니다. */
      expect(targetBytes(call.url)).toBeLessThanOrEqual(MAX_RECENT_REQUEST_TARGET_BYTES);
      seen.push(call);
      sent.push(...refsInCall);
      await act(async () => {
        call.settle(recentBody(refsInCall.map((encoded) => recentItem(byEncoded.get(encoded)!))));
      });
    }

    /* 저장소에 25개가 있어도 20개까지만, 그리고 원래 순서 그대로 나갑니다. */
    expect(sent).toEqual(refs.slice(0, MAX_RECENT).map(encodeRecentRef));
    expect(zeroProbes()).toHaveLength(0);
    expect(seen.length).toBeGreaterThan(1); // 이 크기면 실제로 나뉘어야 합니다
  });

  it("닫으면 공유 신호가 끊겨 남은 덩어리는 나가지 않습니다", async () => {
    seed(makeRefs(20, 200));
    renderPalette();
    openPalette();
    const first = await nextCall(isRecent);
    /* 닫기 전에 나간 것은 **첫 덩어리 하나뿐**입니다 — 예비 probe도 없습니다. */
    expect(refsOf(first).length).toBeGreaterThan(0);
    expect(chunkCalls()).toHaveLength(1);
    expect(zeroProbes()).toHaveLength(0);

    fireEvent.keyDown(document.body, { key: "Escape" });
    await waitFor(() => expect(first.signal.aborted).toBe(true));

    /* 첫 덩어리 응답이 뒤늦게 도착해도 다음 덩어리는 끝내 나가지 않습니다. */
    await act(async () => {
      first.settle(recentBody([]));
    });
    await flush();
    expect(chunkCalls()).toHaveLength(1);
    expect(recentCalls()).toHaveLength(1);
  });

  it("완전히 성공한 해석 뒤에만 사라진 참조를 지웁니다", async () => {
    const refs = makeRefs(3);
    seed(refs);
    renderPalette();
    openPalette();
    const call = await nextCall(isRecent);
    expect(refsOf(call)).toEqual(refs.map(encodeRecentRef));
    /* 서버가 두 번째 항목만 되돌려줬습니다 — 나머지는 삭제·권한 상실·교체입니다. */
    await act(async () => {
      call.settle(recentBody([recentItem(refs[1])]));
    });
    await waitFor(() => expect(stored().map((r) => r.uid)).toEqual(["uid-1"]));
    expect(zeroProbes()).toHaveLength(0);
    /* 정리는 저장소만 바꿉니다 — 이번 열기의 질의 키를 흔들어 다시 물어보지 않습니다. */
    await flush();
    expect(recentCalls()).toHaveLength(1);
    /* 이미 받은 응답은 그대로 계속 보여줍니다. */
    expect(await screen.findByRole("option", { name: /nnnnnnnn-1/ })).toBeInTheDocument();
  });

  it("전부 사라진 경우에도 요청은 한 번이고 뒤따르는 0건 probe가 없습니다", async () => {
    const refs = makeRefs(3);
    seed(refs);
    renderPalette();
    openPalette();
    const call = await nextCall(isRecent);
    expect(refsOf(call)).toHaveLength(3);
    /* 물어본 셋이 전부 해석되지 않았습니다. */
    await act(async () => {
      call.settle(recentBody([]));
    });
    await waitFor(() => expect(stored()).toEqual([]));
    await flush();
    expect(recentCalls()).toHaveLength(1);
    expect(zeroProbes()).toHaveLength(0);
    expect(screen.getByText(/최근 본 리소스가 없습니다/)).toBeInTheDocument();
  });

  it("저장소에 오염된 항목이 섞여도 정상 참조만 실어 보냅니다", async () => {
    const good = makeRefs(2);
    seed([
      good[0],
      { ...good[0], uid: "uid-bad-group", group: "Bad_Group" },
      { ...good[0], uid: "uid-bad-version", version: "1v" },
      { ...good[0], uid: "uid-bad-name", name: "has space" },
      good[1],
    ]);
    renderPalette();
    openPalette();
    const call = await nextCall(isRecent);
    /* 하나라도 섞여 나가면 서버가 그 배치 전체를 400으로 거절합니다. */
    expect(refsOf(call)).toEqual(good.map(encodeRecentRef));
  });

  it("클러스터 A의 참조는 클러스터 B로 나가지 않고, 그대로 남습니다", async () => {
    const seoul = makeRefs(2, 8, "prod-seoul");
    const tokyo = makeRefs(1, 8, "stage-tokyo").map((r) => ({ ...r, uid: "uid-tokyo" }));
    seed([...tokyo, ...seoul]);

    const client = renderPalette();
    openPalette();
    const first = await nextCall(isRecent);
    expect(first.url.pathname).toBe("/api/v1/clusters/prod-seoul/resources/recent");
    expect(refsOf(first)).toEqual(seoul.map(encodeRecentRef));

    /* 화면에서 클러스터를 바꿉니다. 옛 클러스터의 참조가 새 엔드포인트로 가면 안 됩니다. */
    act(() => {
      client.switchCluster("stage-tokyo");
    });
    await waitFor(() => expect(first.signal.aborted).toBe(true));
    const second = await nextCall(isRecent, [first]);
    expect(second.url.pathname).toBe("/api/v1/clusters/stage-tokyo/resources/recent");
    expect(refsOf(second)).toEqual(tokyo.map(encodeRecentRef));

    /* 두 클러스터의 참조가 모두 저장소에 남아 있습니다. */
    expect(stored().map((r) => r.clusterId).sort()).toEqual(["prod-seoul", "prod-seoul", "stage-tokyo"]);
  });

  it("입력을 시작하면 최근 항목 요청 그 하나가 끊깁니다", async () => {
    seed(makeRefs(2));
    renderPalette();
    openPalette();
    const call = await nextCall(isRecent);
    expect(call.signal.aborted).toBe(false);

    /* 검색으로 넘어간 순간부터 이 응답은 화면에 쓰이지 않습니다. */
    fireEvent.change(screen.getByRole("combobox"), { target: { value: "pay" } });
    await waitFor(() => expect(call.signal.aborted).toBe(true));
  });

  it("끊긴 해석으로는 저장소를 지우지 않습니다", async () => {
    const refs = makeRefs(3);
    seed(refs);
    renderPalette();
    openPalette();
    const call = await nextCall(isRecent);
    expect(refsOf(call)).toHaveLength(refs.length);

    fireEvent.keyDown(document.body, { key: "Escape" });
    await waitFor(() => expect(call.signal.aborted).toBe(true));
    await act(async () => {
      call.settle(recentBody([]));
    });
    await flush();
    /* 중간에 끊긴 결과로 지우면 살아 있는 항목을 잃습니다. */
    expect(stored().map((r) => r.uid)).toEqual(["uid-0", "uid-1", "uid-2"]);
  });

  it("열 때마다 다시 확인하고, 도는 동안 옛 제목을 보여주지 않습니다", async () => {
    const refs = makeRefs(1);
    seed(refs);
    renderPalette();

    openPalette();
    const first = await nextCall(isRecent);
    expect(refsOf(first)).toEqual(refs.map(encodeRecentRef));
    await act(async () => {
      first.settle(recentBody([recentItem(refs[0])]));
    });
    expect(await screen.findByRole("option", { name: /nnnnnnnn-0/ })).toBeInTheDocument();

    fireEvent.keyDown(document.body, { key: "Escape" });
    openPalette();

    /* 두 번째 열기에서 저장소를 다시 읽고 다시 물어봅니다 — 그 답이 오기 전에는
       옛 제목을 쓰지 않고, 예비 0건 probe도 끼지 않습니다. */
    const second = await nextCall(isRecent, [first]);
    expect(refsOf(second)).toEqual(refs.map(encodeRecentRef));
    expect(screen.queryByRole("option", { name: /nnnnnnnn-0/ })).not.toBeInTheDocument();
    expect(screen.getByText(/다시 확인하는 중/)).toBeInTheDocument();

    /* 서버가 다시 확인해 준 뒤에야 제목이 돌아옵니다 — 그 제목의 근거는 이번 응답입니다. */
    await act(async () => {
      second.settle(recentBody([recentItem(refs[0])]));
    });
    expect(await screen.findByRole("option", { name: /nnnnnnnn-0/ })).toBeInTheDocument();
    expect(zeroProbes()).toHaveLength(0);
  });
});
