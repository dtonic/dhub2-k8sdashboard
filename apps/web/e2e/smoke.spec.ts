import { expect, test } from "@playwright/test";
import AxeBuilder from "@axe-core/playwright";
import { trackFailures, waitForData } from "./helpers";

/**
 * 모든 화면이 열리고, 이동 중 4xx/5xx가 없어야 합니다.
 * Light/Dark 두 모드에서 모두 돕니다 — 토큰이 한쪽 모드에만 정의된 실수를 잡습니다.
 */
const ROUTES = [
  { path: "/?range=1h", title: "Cluster Overview" },
  { path: "/namespaces?range=1h", title: "Namespaces" },
  { path: "/namespaces/payments?range=1h", title: "payments" },
  { path: "/workloads/Deployment/payments-api?ns=payments&range=1h", title: "payments-api" },
  { path: "/logs?range=1h", title: "Logs Explorer" },
  { path: "/topology?range=1h", title: "Pod Topology" },
  { path: "/alerts?range=7d", title: "Alerts" },
];

for (const r of ROUTES) {
  test(`화면이 열린다: ${r.path}`, async ({ page }) => {
    const failures = trackFailures(page);
    await page.goto(r.path);
    await waitForData(page);
    await expect(page.locator("h1")).toContainText(r.title);
    expect(failures, `API 실패 응답: ${failures.join(", ")}`).toEqual([]);
  });
}

test("본문 건너뛰기 링크가 첫 탭 정지점이다", async ({ page }) => {
  await page.goto("/?range=1h");
  await waitForData(page);
  await page.keyboard.press("Tab");
  await expect(page.locator(":focus")).toHaveText("본문으로 건너뛰기");
  await page.keyboard.press("Enter");
  expect(new URL(page.url()).hash).toBe("#main");
});

test("접근성 이름 없는 컨트롤이 없다", async ({ page }) => {
  await page.goto("/?range=1h");
  await waitForData(page);
  const missing = await page.evaluate(() => {
    const out: string[] = [];
    for (const el of document.querySelectorAll('button, select, a[href], [role="button"]')) {
      const label =
        el.getAttribute("aria-label") ||
        el.textContent?.trim() ||
        (el.id && document.querySelector(`label[for="${el.id}"]`)?.textContent?.trim()) ||
        "";
      if (!label) out.push(el.tagName + "." + String(el.className).split(" ")[0]);
    }
    return out;
  });
  expect(missing).toEqual([]);
  await expect(page.locator("main")).toHaveCount(1);
  await expect(page.locator("nav")).not.toHaveCount(0);
});

test("app shell and common controls have no serious accessibility violations", async ({ page }) => {
  await page.goto("/?range=1h");
  await waitForData(page);

  const { violations } = await new AxeBuilder({ page }).analyze();
  const serious = violations.filter(({ impact }) => impact === "critical" || impact === "serious");
  expect(serious, JSON.stringify(serious, null, 2)).toEqual([]);
});
