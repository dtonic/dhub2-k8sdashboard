import { expect, test } from "@playwright/test";
import { trackApi, trackFailures, waitForData } from "./helpers";

test.describe("Pod Topology", () => {
  test("A→B와 B→A가 별도 선으로 분리된다", async ({ page }) => {
    await page.goto("/topology?range=1h");
    await waitForData(page);

    const labels = await page
      .locator("g.ds-topo-edge")
      .evaluateAll((gs) => gs.map((g) => g.getAttribute("aria-label") ?? ""));
    const pair = labels.filter((l) => l.includes("payments에서 auth") || l.includes("auth에서 payments"));
    expect(pair).toHaveLength(2);
    expect(await page.locator("g.ds-topo-edge").count()).toBeGreaterThan(8);
  });

  test("방향 캡슐 라벨이 서로 겹치지 않는다", async ({ page }) => {
    await page.goto("/topology?range=1h");
    await waitForData(page);

    const overlaps = await page.evaluate(() => {
      const rects = [...document.querySelectorAll(".ds-topo-edge__cap")].map((r) => r.getBoundingClientRect());
      let hit = 0;
      for (let i = 0; i < rects.length; i++) {
        for (let j = i + 1; j < rects.length; j++) {
          const a = rects[i]!;
          const b = rects[j]!;
          if (!(a.right < b.left || a.left > b.right || a.bottom < b.top || a.top > b.bottom)) hit++;
        }
      }
      return hit;
    });
    expect(overlaps).toBe(0);
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
    await page.locator("g.ds-topo-node").nth(2).dispatchEvent("click");
    await waitForData(page);
    await expect(page.locator("h1")).not.toBeEmpty();
    expect(failures, `deep link 실패: ${failures.join(", ")}`).toEqual([]);
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
