import { expect, test } from "@playwright/test";
import { trackApi, waitForData } from "./helpers";

/** 이슈 #14 완료 기준을 그대로 테스트로 옮긴 것입니다. */
test.describe("Cluster Overview (#14)", () => {
  test("초기 로딩에서 N+1 요청이 발생하지 않는다", async ({ page }) => {
    const api = trackApi(page);
    await page.goto("/?range=1h");
    await waitForData(page);

    /* 화면 전체가 /scope + /overview 두 번이어야 합니다. 위젯마다 훅을 만들면 여기서 깨집니다. */
    expect(api.matching(/\/overview/)).toHaveLength(1);
    expect(api.count()).toBeLessThanOrEqual(2);
  });

  test("시간 범위를 바꾸면 요청 1건만 늘고 페이지는 다시 마운트되지 않는다", async ({ page }) => {
    const api = trackApi(page);
    await page.goto("/?range=1h");
    await waitForData(page);

    const before = api.count();
    const h1 = await page.locator("h1").elementHandle();
    await page.getByRole("button", { name: "7일", exact: true }).click();
    await waitForData(page);

    expect(api.since(before).filter((c) => c.includes("/overview"))).toHaveLength(1);
    /* DOM 노드가 유지되어야 "데이터만 갱신"입니다. */
    expect(await page.evaluate((el) => el === document.querySelector("h1"), h1)).toBe(true);
  });

  test("데이터 없음 · 권한 없음 · upstream 장애가 서로 다르게 보인다", async ({ page }) => {
    await page.goto("/?scenario=empty&range=1h");
    await waitForData(page);
    await expect(page.getByText("이상 엔티티가 없습니다")).toBeVisible();

    await page.goto("/?scenario=forbidden&range=1h");
    await waitForData(page);
    await expect(page.getByText("권한이 없습니다").first()).toBeVisible();
    await expect(page.getByText("조회가 거절되었습니다").first()).toBeVisible();

    await page.goto("/?scenario=degraded&range=1h");
    await waitForData(page);
    await expect(page.getByText("Alertmanager 응답 없음")).toBeVisible();
    /* 부분 장애여도 다른 패널은 살아 있어야 합니다. */
    await expect(page.getByText("CPU 사용률")).toBeVisible();
    await expect(page.getByText("GreptimeDB").first()).toBeVisible();
  });

  test("접근 불가한 클러스터는 화면 전체가 권한 안내다", async ({ page }) => {
    await page.goto("/?cluster=prod-frankfurt&range=1h");
    await waitForData(page);
    await expect(page.getByText("권한이 없습니다").first()).toBeVisible();
    await expect(page.getByText("이상 엔티티 Top N")).toHaveCount(0);
  });

  test("이상 엔티티까지 1클릭으로 이동한다", async ({ page }) => {
    await page.goto("/?range=1h");
    await waitForData(page);
    /* 이상 목록과 이벤트 피드 양쪽에 같은 이름이 있습니다. 목록 쪽을 씁니다. */
    await page.getByRole("link", { name: "batch-sync-qq81z" }).first().click();
    await waitForData(page);
    await expect(page.locator("h1")).toContainText("batch-sync");
    expect(new URL(page.url()).searchParams.get("ns")).toBe("payments");
  });
  test("range change aborts the previous overview fetch and renders the latest result", async ({ page }) => {
    await page.addInitScript(() => {
      const originalFetch = window.fetch;
      const records: Array<{ url: string; aborted: boolean }> = [];
      Object.defineProperty(window, "__overviewFetchSignals", { value: records });
      window.fetch = ((input: RequestInfo | URL, init?: RequestInit) => {
        const url = typeof input === "string" ? input : input instanceof URL ? input.href : input.url;
        const signal = init?.signal ?? (input instanceof Request ? input.signal : undefined);
        if (url.includes("/overview")) {
          const record = { url, aborted: signal?.aborted ?? false };
          records.push(record);
          signal?.addEventListener("abort", () => { record.aborted = true; }, { once: true });
        }
        return originalFetch(input, init);
      }) as typeof window.fetch;
    });

    await page.goto("/?scenario=slow&range=1h");
    await page.waitForFunction(() => ((window as any).__overviewFetchSignals?.length ?? 0) === 1);
    await page.getByRole("button", { name: "7일", exact: true }).click();
    await page.waitForFunction(() => (window as any).__overviewFetchSignals?.[0]?.aborted === true);
    await waitForData(page);

    const requests = await page.evaluate(
      () => (window as any).__overviewFetchSignals as Array<{ url: string; aborted: boolean }>,
    );
    expect(requests).toHaveLength(2);
    expect(requests[0]!.url).toContain("range=1h");
    expect(requests[0]!.aborted).toBe(true);
    expect(requests[1]!.url).toContain("range=7d");
    await expect(page.locator(".page__subtitle")).toContainText("최근 7일");
    await expect(page.locator(".panel__subtitle", { hasText: "step" }).first()).toContainText("step 15분");
  });
});
