import { expect, test } from "@playwright/test";
import AxeBuilder from "@axe-core/playwright";
import { trackApi, waitForData } from "./helpers";

test.describe("Embedded dashboards (#18)", () => {
  test("standard file is auto-discovered in nav and generic route uses one overview request", async ({ page }) => {
    const api = trackApi(page);
    await page.goto("/dashboards/cluster-operations?range=1h&refresh=0");
    await waitForData(page);
    await expect(page.getByRole("link", { name: "Cluster Operations" })).toBeVisible();
    await expect(page.getByRole("heading", { name: "Cluster Operations" })).toBeVisible();
    const controlOrder = await page.evaluate(() => {
      const scope = document.querySelector("#scope-cluster")!;
      const range = document.querySelector('[role="group"][aria-label="시간 범위"]')!;
      const refresh = document.querySelector("#refresh-interval")!;
      return [scope, range, refresh].map((node) => Array.from(document.querySelectorAll("select, [role=group]" )).indexOf(node));
    });
    expect(controlOrder[0]).toBeLessThan(controlOrder[1]!);
    expect(controlOrder[1]).toBeLessThan(controlOrder[2]!);
    for (const title of ["Nodes Ready", "Pods Running", "CPU Usage", "Memory Usage", "Unhealthy Workloads", "Recent Events"])
      await expect(page.getByText(title, { exact: true }).first()).toBeVisible();
    expect(api.matching(/\/overview/)).toHaveLength(1);
    expect(api.count()).toBeLessThanOrEqual(2);
  });

  test("unknown dashboard is a local error and does not request overview", async ({ page }) => {
    const api = trackApi(page);
    await page.goto("/dashboards/not-committed?refresh=0");
    await waitForData(page);
    await expect(page.getByText("Dashboard definition is missing or invalid.")).toBeVisible();
    expect(api.matching(/\/overview/)).toHaveLength(0);
  });

  test("large-row widgets keep the DOM bounded through the existing virtual table", async ({ page }) => {
    await page.goto("/dashboards/cluster-operations?range=1h&refresh=0");
    await waitForData(page);
    expect(await page.locator(".vtable__body tbody tr").count()).toBeLessThanOrEqual(40);
  });

  test("empty and degraded stale Section states stay local and accessible", async ({ page }) => {
    await page.goto("/dashboards/cluster-operations?scenario=empty&refresh=0");
    await waitForData(page);
    await expect(page.getByText("No unhealthy workloads")).toBeVisible();
    await expect(page.getByText("No recent events")).toBeVisible();

    await page.goto("/dashboards/cluster-operations?scenario=degraded&refresh=0");
    await waitForData(page);
    await expect(page.getByText(/GreptimeDB/).first()).toBeVisible();
    await expect(page.getByText(/Quickwit/).first()).toBeVisible();
    const { violations } = await new AxeBuilder({ page }).analyze();
    expect(violations.filter(({ impact }) => impact === "critical" || impact === "serious")).toEqual([]);
  });

  test("range changes abort the previous aggregate request", async ({ page }) => {
    await page.addInitScript(() => {
      const originalFetch = window.fetch;
      const records: Array<{ url: string; aborted: boolean }> = [];
      Object.defineProperty(window, "__dashboardFetchSignals", { value: records });
      window.fetch = ((input: RequestInfo | URL, init?: RequestInit) => {
        const url = typeof input === "string" ? input : input instanceof URL ? input.href : input.url;
        const signal = init?.signal ?? (input instanceof Request ? input.signal : undefined);
        if (url.includes("/overview")) {
          const record = { url, aborted: signal?.aborted ?? false }; records.push(record);
          signal?.addEventListener("abort", () => { record.aborted = true; }, { once: true });
        }
        return originalFetch(input, init);
      }) as typeof window.fetch;
    });
    await page.goto("/dashboards/cluster-operations?scenario=slow&range=1h&refresh=0");
    await page.waitForFunction(() => ((window as any).__dashboardFetchSignals?.length ?? 0) === 1);
    await page.getByRole("button", { name: "7일", exact: true }).click();
    await page.waitForFunction(() => (window as any).__dashboardFetchSignals?.[0]?.aborted === true);
    await waitForData(page);
    const records = await page.evaluate(() => (window as any).__dashboardFetchSignals as Array<{ url: string; aborted: boolean }>);
    expect(records).toHaveLength(2); expect(records[0]!.aborted).toBe(true); expect(records[1]!.url).toContain("range=7d");
  });

  test("a missing query series fails only its widget", async ({ page }) => {
    await page.addInitScript(() => {
      const originalFetch = window.fetch;
      window.fetch = (async (input: RequestInfo | URL, init?: RequestInit) => {
        const response = await originalFetch(input, init);
        const url = typeof input === "string" ? input : input instanceof URL ? input.href : input.url;
        if (!url.includes("/overview")) return response;
        const body = await response.clone().json();
        const cpu = body.trends?.data?.find((panel: { id: string }) => panel.id === "cpu");
        if (cpu) cpu.series = cpu.series.filter((series: { key: string }) => series.key !== "requested");
        return new Response(JSON.stringify(body), { status: response.status, headers: { "content-type": "application/json" } });
      }) as typeof window.fetch;
    });
    await page.goto("/dashboards/cluster-operations?range=1h&refresh=0");
    await waitForData(page);
    await expect(page.getByText("The dashboard queryRef is not present in the overview response.")).toBeVisible();
    await expect(page.getByText("16/18")).toBeVisible();
  });
});
