import { expect, test } from "@playwright/test";
import { waitForData } from "./helpers";

/** 이슈 #16 완료 기준. */
test.describe("Logs Explorer (#16)", () => {
  test("대량 결과를 한 번에 적재하지 않는다", async ({ page }) => {
    await page.goto("/logs?range=1h");
    await waitForData(page);

    /* 여러 페이지를 불러와도 DOM 라인 수는 화면 크기에 묶여 있어야 합니다. */
    for (let i = 0; i < 4; i++) {
      await page.locator(".loglist__viewport").evaluate((el) => {
        el.scrollTop = el.scrollHeight;
      });
      await page.waitForTimeout(700);
    }
    const footer = await page.locator(".loglist__footer").innerText();
    expect(footer).toMatch(/불러온 줄 [45]00/);
    expect(await page.locator(".logline").count()).toBeLessThan(80);
  });

  test("커서 페이징에 중복·누락이 없다", async ({ page }) => {
    await page.goto("/logs?range=1h");
    await waitForData(page);

    const result = await page.evaluate(async () => {
      const seen = new Set<string>();
      let total = 0;
      let dup = 0;
      let cursor = "";
      for (let i = 0; i < 8; i++) {
        const u = new URL("/api/v1/clusters/prod-seoul/logs", location.origin);
        u.searchParams.set("range", "1h");
        if (cursor) u.searchParams.set("cursor", cursor);
        const r = await (await fetch(u)).json();
        for (const l of r.lines.data ?? []) {
          total++;
          if (seen.has(l.id)) dup++;
          seen.add(l.id);
        }
        if (!r.cursor.next) break;
        cursor = r.cursor.next;
      }
      return { total, unique: seen.size, dup };
    });

    expect(result.dup).toBe(0);
    expect(result.unique).toBe(result.total);
    expect(result.total).toBeGreaterThan(400);
  });

  test("마스킹은 서버에서 이뤄지고 응답에 원문이 없다", async ({ page }) => {
    await page.goto("/logs?range=30d&levels=ERROR");
    await waitForData(page);

    const check = await page.evaluate(async () => {
      const r = await (await fetch("/api/v1/clusters/prod-seoul/logs?range=30d&levels=ERROR")).json();
      const lines = r.lines.data ?? [];
      const masked = lines.filter((l: { masked: unknown[] }) => l.masked.length > 0);
      return {
        maskedCount: masked.length,
        /* 마스킹된 줄은 반드시 가림 문자를 포함하고, 스팬 정보만 함께 옵니다. */
        allHidden: masked.every((l: { message: string }) => l.message.includes("•")),
        /* 원문 필드가 따로 실려오면 안 됩니다. */
        noRawField: masked.every((l: object) => !("raw" in l) && !("original" in l)),
      };
    });

    expect(check.maskedCount).toBeGreaterThan(0);
    expect(check.allHidden).toBe(true);
    expect(check.noRawField).toBe(true);
    await expect(page.locator(".logline__masked").first()).toBeVisible();
  });

  test("차트 구간을 선택하면 그 범위로 조회가 좁혀진다", async ({ page }) => {
    await page.goto("/logs?range=1d");
    await waitForData(page);

    const box = (await page.locator(".panel svg").first().boundingBox())!;
    await page.mouse.move(box.x + box.width * 0.6, box.y + box.height * 0.5);
    await page.mouse.down();
    await page.mouse.move(box.x + box.width * 0.85, box.y + box.height * 0.5, { steps: 10 });
    await page.mouse.up();
    await waitForData(page);

    const url = new URL(page.url());
    expect(url.searchParams.get("from")).toBeTruthy();
    expect(url.searchParams.get("to")).toBeTruthy();
    await expect(page.locator(".page__subtitle")).toContainText("선택 구간");
  });

  test("Pod 상세에서 로그로 이동할 때 Scope와 시간 범위가 유지된다", async ({ page }) => {
    await page.goto("/logs?range=7d");
    await waitForData(page);
    await page.locator(".logline__main").first().click();
    await page.locator(".logline__meta a").first().click();
    await waitForData(page);

    await page.getByRole("link", { name: /로그 열기/ }).click();
    await waitForData(page);

    const u = new URL(page.url());
    expect(u.pathname).toBe("/logs");
    expect(u.searchParams.get("range")).toBe("7d");
    expect(u.searchParams.get("ns")).toBeTruthy();
    expect(u.searchParams.get("uid")).toBeTruthy();
  });
});
