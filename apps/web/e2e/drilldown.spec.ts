import { expect, test } from "@playwright/test";
import { trackApi, waitForData } from "./helpers";

/** 이슈 #15 완료 기준. */
test.describe("Drill-down (#15)", () => {
  test("권한 없는 Namespace를 직접 열어도 데이터가 노출되지 않는다", async ({ page }) => {
    await page.goto("/namespaces/media?cluster=prod-tokyo&range=1h");
    await waitForData(page);
    await expect(page.getByText("권한이 없습니다").first()).toBeVisible();
    /* 목록이 부분적으로도 그려지면 안 됩니다. */
    await expect(page.locator(".vtable__body tbody tr")).toHaveCount(0);
    await expect(page.getByText("Workload 상태")).toHaveCount(0);
  });

  test("Workload가 많아도 가상 스크롤로 일부만 렌더한다", async ({ page }) => {
    await page.goto("/namespaces/payments?range=1h");
    await waitForData(page);

    const footer = await page.locator(".vtable__footer").innerText();
    expect(footer).toContain("240");
    const rendered = await page.locator(".vtable__body tbody tr").count();
    expect(rendered).toBeLessThanOrEqual(30);
    expect(rendered).toBeGreaterThan(5);

    /* 스크롤해도 렌더 행 수가 늘지 않아야 가상화가 동작하는 것입니다. */
    await page.locator(".vtable__viewport").evaluate((el) => {
      el.scrollTop = 4000;
    });
    await page.waitForTimeout(200);
    expect(await page.locator(".vtable__body tbody tr").count()).toBeLessThanOrEqual(30);
  });

  test("갱신과 범위 변경이 스크롤·필터 선택을 지우지 않는다", async ({ page }) => {
    await page.goto("/namespaces/payments?range=1h");
    await waitForData(page);

    await page.locator(".vtable__viewport").evaluate((el) => {
      el.scrollTop = 3000;
    });
    await page.waitForTimeout(150);

    await page.getByRole("button", { name: "지금 갱신" }).click();
    await waitForData(page);
    expect(await page.locator(".vtable__viewport").evaluate((el) => el.scrollTop)).toBe(3000);

    await page.getByRole("button", { name: "7일", exact: true }).click();
    await waitForData(page);
    expect(await page.locator(".vtable__viewport").evaluate((el) => el.scrollTop)).toBe(3000);

    await page.locator(".chip", { hasText: "CrashLoopBackOff" }).click();
    await waitForData(page);
    expect(new URL(page.url()).searchParams.get("issues")).toContain("CrashLoopBackOff");
    await page.getByRole("button", { name: "지금 갱신" }).click();
    await waitForData(page);
    await expect(page.locator('.chip[aria-pressed="true"]')).toHaveText(/CrashLoopBackOff/);
  });

  test("Deployment → ReplicaSet → Pod 관계가 세대까지 표시된다", async ({ page }) => {
    await page.goto("/workloads/Deployment/payments-api?ns=payments&range=1h");
    await waitForData(page);

    const chain = page.locator(".owner-chain__item");
    await expect(chain).toHaveCount(2);
    await expect(page.locator(".owner-chain__item--current")).toHaveCount(1);
    await expect(page.getByText("현재 세대")).toBeVisible();
    await expect(page.getByText("이전 세대")).toBeVisible();
  });

  test("이름이 같아도 UID가 다른 Pod는 다른 인스턴스로 구분된다", async ({ page }) => {
    await page.goto("/workloads/CronJob/batch-sync?ns=payments&range=1h");
    await waitForData(page);

    const rows = await page.locator(".vtable__body tbody tr").evaluateAll((trs) =>
      trs.map((tr) => {
        const a = tr.querySelector("td:first-child a");
        return {
          name: a?.textContent?.trim() ?? "",
          uid: new URLSearchParams((a?.getAttribute("href") ?? "").split("?")[1]).get("uid") ?? "",
        };
      }),
    );
    expect(rows.length).toBeGreaterThan(1);
    expect(new Set(rows.map((r) => r.name)).size).toBe(1);
    expect(new Set(rows.map((r) => r.uid)).size).toBe(rows.length);

    /* 두 인스턴스의 상세 화면이 서로 달라야 합니다. */
    await page.goto(`/pods/${encodeURIComponent(rows[0]!.name)}?ns=payments&uid=${rows[0]!.uid}&range=1h`);
    await waitForData(page);
    const a = await page.locator(".page__subtitle").innerText();
    await page.goto(`/pods/${encodeURIComponent(rows[1]!.name)}?ns=payments&uid=${rows[1]!.uid}&range=1h`);
    await waitForData(page);
    const b = await page.locator(".page__subtitle").innerText();
    expect(a).not.toBe(b);
  });

  test("각 상세 화면은 요청 하나로 그려진다", async ({ page }) => {
    const api = trackApi(page);
    await page.goto("/workloads/Deployment/payments-api?ns=payments&range=1h");
    await waitForData(page);
    expect(api.matching(/\/workloads\//)).toHaveLength(1);
  });
});

/** 이슈 #2 완료 기준: 목록에서는 namespace가 아니라 이름으로 찾고 URL 상태가 새지 않습니다. */
test.describe("Namespace 목록 URL 상태 (#2)", () => {
  test("stale ns를 제거하고 검색을 갱신·범위 변경 뒤에도 보존한다", async ({ page }) => {
    const api = trackApi(page);
    await page.goto("/namespaces?ns=media&q=pay&range=1h&refresh=10000");
    await waitForData(page);

    let url = new URL(page.url());
    expect(url.searchParams.get("ns")).toBeNull();
    expect(url.searchParams.get("q")).toBe("pay");
    await expect(page.getByRole("combobox", { name: "네임스페이스" })).toHaveCount(0);

    const search = page.getByRole("searchbox", { name: "Namespace 이름 검색" });
    await expect(search).toHaveValue("pay");
    await expect(page.getByRole("link", { name: "payments", exact: true })).toBeVisible();
    await expect(page.getByRole("link", { name: "media", exact: true })).toHaveCount(0);

    const initialRequests = api.matching(/\/namespaces\?/).length;
    await expect
      .poll(() => api.matching(/\/namespaces\?/).length, { timeout: 15_000 })
      .toBeGreaterThan(initialRequests);
    await expect(search).toHaveValue("pay");
    expect(new URL(page.url()).searchParams.get("q")).toBe("pay");

    await page.getByRole("button", { name: "7일", exact: true }).click();
    await waitForData(page);
    url = new URL(page.url());
    expect(url.searchParams.get("range")).toBe("7d");
    expect(url.searchParams.get("q")).toBe("pay");
    await expect(search).toHaveValue("pay");

    await page.getByRole("link", { name: "payments", exact: true }).click();
    await waitForData(page);
    url = new URL(page.url());
    expect(url.pathname).toBe("/namespaces/payments");
    expect(url.searchParams.get("ns")).toBeNull();
    expect(url.searchParams.get("q")).toBeNull();
    expect(url.searchParams.get("range")).toBe("7d");
    await expect(page.getByRole("heading", { name: "payments", exact: true })).toBeVisible();
    await expect(page.getByRole("combobox", { name: "네임스페이스" })).toHaveCount(0);
  });
});
