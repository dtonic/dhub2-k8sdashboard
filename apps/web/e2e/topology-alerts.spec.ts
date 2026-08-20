import { expect, test } from "@playwright/test";
import { trackApi, trackFailures, waitForData } from "./helpers";

test.describe("Pod Topology", () => {
  test("A→B와 B→A가 별도 선으로 분리된다", async ({ page }) => {
    await page.goto("/topology?range=1h");
    await waitForData(page);
    // 방향별 별도 엣지는 React Flow 엣지 path 개수로 확인합니다(캡슐 제거 후).
    const edgeCount = await page.locator(".react-flow__edge").count();
    expect(edgeCount).toBeGreaterThan(8);
  });

  test("프로토콜은 선이 아니라 노드 카드의 In/Out으로 표기된다", async ({ page }) => {
    await page.goto("/topology?range=1h");
    await waitForData(page);
    // 선에는 프로토콜 캡슐이 없어야 한다.
    expect(await page.locator(".topo-edge__cap").count()).toBe(0);
    // 노드 카드에는 In/Out 프로토콜 요약이 있어야 한다.
    const protoRows = await page.locator(".topo-node__proto-row").count();
    expect(protoRows).toBeGreaterThan(0);
    await expect(page.locator(".topo-node__proto-dir").first()).toHaveText(/In|Out/);
    await expect(page.locator(".topo-node__proto").first()).toHaveText(/HTTP|gRPC|TCP|UDP/);
  });

  test("엣지 시계열은 선택했을 때만 조회한다", async ({ page }) => {
    const api = trackApi(page);
    await page.goto("/topology?range=1h");
    await waitForData(page);
    expect(api.matching(/\/series/)).toHaveLength(0);

    await page.getByRole("button", { name: /시계열 차트로 보기/ }).click();
    await waitForData(page);
    expect(api.matching(/\/series/)).toHaveLength(1);
  });

  test("노드에서 Pod 상세로 이동해도 API 실패가 없다", async ({ page }) => {
    const failures = trackFailures(page);
    await page.goto("/topology?range=1h");
    await waitForData(page);
    await page.locator(".topo-node").nth(2).click();
    await waitForData(page);
    await expect(page.locator("h1")).not.toBeEmpty();
    expect(failures, `deep link 실패: ${failures.join(", ")}`).toEqual([]);
  });

  test("Route를 클릭하면 그 경로의 실제 로그를 조회한다 (#31)", async ({ page }) => {
    await page.goto("/topology?range=1h");
    await waitForData(page);
    // 첫 선이 자동 선택되어 방향 상세 표가 열려 있다. Route 이름을 클릭한다.
    const routeBtn = page.locator(".topo-route-toggle").first();
    await routeBtn.click();
    await waitForData(page);
    // 실제 로그 뷰(mock 로그)가 펼쳐지고 hex 덤프가 아니어야 한다.
    await expect(page.locator(".topo-route-logs, .topo-route-logs__hint")).toBeVisible();
    expect(await page.locator(".topo-hex").count()).toBe(0);
  });

  test("배치 편집: 저장한 좌표가 다시 조회해도 유지된다 (#28)", async ({ page }) => {
    /* 화면(fitView) 좌표가 아니라 **flow 좌표**(노드 transform)를 비교합니다 —
       fitView가 뷰포트를 재정규화하므로 boundingBox로는 저장 여부를 알 수 없습니다. */
    const flowPos = () =>
      page
        .locator(".react-flow__node")
        .first()
        .evaluate((el) => {
          const m = /translate\(([-\d.]+)px,\s*([-\d.]+)px\)/.exec((el as HTMLElement).style.transform ?? "");
          return m ? { x: parseFloat(m[1]!), y: parseFloat(m[2]!) } : null;
        });

    await page.goto("/topology?range=1h");
    await waitForData(page);

    await page.getByRole("button", { name: "배치 편집" }).click();
    const node = page.locator(".react-flow__node").first();
    const before = await flowPos();
    expect(before).not.toBeNull();

    /* 노드를 드래그해 옮긴 뒤 저장합니다. */
    const box = (await node.boundingBox())!;
    await node.hover();
    await page.mouse.down();
    await page.mouse.move(box.x + box.width / 2 + 140, box.y + box.height / 2 + 90, { steps: 8 });
    await page.mouse.up();
    const afterDrag = await flowPos();
    expect(afterDrag).not.toBeNull();
    expect(Math.hypot(afterDrag!.x - before!.x, afterDrag!.y - before!.y)).toBeGreaterThan(40);

    await page.getByRole("button", { name: "배치 저장" }).click();
    await waitForData(page);

    /* 저장 후 재조회(다른 사용자 화면과 같은 경로)에서도 flow 좌표가 유지됩니다. */
    await page.reload();
    await waitForData(page);
    const after = await flowPos();
    expect(after).not.toBeNull();
    expect(Math.abs(after!.x - afterDrag!.x)).toBeLessThan(2);
    expect(Math.abs(after!.y - afterDrag!.y)).toBeLessThan(2);
  });

  test("500개 워크로드를 절단 없이 10초 안에 렌더한다 (#3)", async ({ page }) => {
    const startedAt = Date.now();
    await page.goto("/topology?range=1h&refresh=0&scenario=large-topology");
    await waitForData(page);

    await expect(page.locator(".react-flow__node")).toHaveCount(500);
    await expect(page.getByText("워크로드 노드 500개 · Pod 1,000개를 접어서 표시", { exact: true })).toBeVisible();
    expect(Date.now() - startedAt).toBeLessThan(10_000);
  });
});

test.describe("Alerts (#17)", () => {
  test("Active와 Resolved가 같은 형식으로 표시된다", async ({ page }) => {
    await page.goto("/alerts?range=7d");
    await waitForData(page);

    const headers = () => page.locator(".grid--split table thead th").allInnerTexts();
    const firing = await headers();
    expect(await page.locator(".grid--split tbody tr").count()).toBeGreaterThan(0);

    await page.locator(".chip", { hasText: "해소됨" }).click();
    await waitForData(page);
    const resolved = await headers();

    /* 상태만 다르고 표 형식은 같아야 합니다. */
    expect(resolved.slice(0, 3)).toEqual(firing.slice(0, 3));
    expect(new URL(page.url()).searchParams.get("tab")).toBe("resolved");
  });

  test("Alert에서 관련 Workload/Pod와 로그로 이동할 수 있다", async ({ page }) => {
    await page.goto("/alerts?range=7d");
    await waitForData(page);
    await page.locator(".grid--split tbody tr").nth(2).click();
    await waitForData(page);

    await expect(page.getByRole("link", { name: /관련 대상 상세/ })).toBeVisible();
    await expect(page.getByRole("link", { name: /관련 로그/ })).toBeVisible();

    await page.getByRole("link", { name: /관련 대상 상세/ }).click();
    await waitForData(page);
    await expect(page.locator("h1")).not.toBeEmpty();
  });

  test("Alert backend 장애가 화면 전체를 실패시키지 않는다", async ({ page }) => {
    await page.goto("/alerts?range=7d&scenario=degraded");
    await waitForData(page);

    await expect(page.locator("h1")).toContainText("Alerts");
    await expect(page.getByText("조회 전용")).toBeVisible();
    await expect(page.getByText("Alertmanager").first()).toBeVisible();
    await expect(page.getByText("불러오지 못했습니다")).toHaveCount(0);
  });

  test("중복 grouping 기준이 화면에 노출된다", async ({ page }) => {
    await page.goto("/alerts?range=7d");
    await waitForData(page);
    await expect(page.getByText(/Grouping 기준/)).toBeVisible();
    await expect(page.getByText(/alertname \+ namespace \+ workload/)).toBeVisible();
  });
});
