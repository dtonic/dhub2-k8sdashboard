import { expect, test } from "@playwright/test";
import { FIXTURE } from "../playwright.integration.config";
import { authedPage } from "./helpers";

/**
 * 데이터소스 부분 장애 UX (#22): 픽스처 인스턴스별로 GreptimeDB · Quickwit ·
 * Alertmanager를 독립적으로 내리고, Kubernetes 기반 화면은 그대로 쓸 수 있는지 ·
 * 죽은 소스의 섹션만 올바른 출처/사유로 강등되는지 검증합니다. (ADR 0002)
 */
test.describe("데이터소스 중단에도 화면은 산다 (#22)", () => {
  test("GreptimeDB 중단: 메트릭만 강등되고 Kubernetes 화면은 유지된다", async ({ browser }) => {
    const { context, page } = await authedPage(browser, FIXTURE.greptimeOutage, "operator", ["platform.admin"]);
    try {
      await page.goto("/?range=1h");
      /* 죽은 소스의 섹션: 출처가 명시된 degraded. */
      await expect(page.getByText("GreptimeDB 응답 없음").first()).toBeVisible();
      /* Kubernetes 이상 엔티티·알림 섹션은 값이 그대로 살아 있습니다. */
      await expect(page.getByRole("link", { name: "payments-api-7f-bbb" }).first()).toBeVisible();
      await expect(page.getByText("Alertmanager 응답 없음")).toHaveCount(0);
      /* drilldown도 계속 동작합니다. */
      await page.goto("/namespaces/payments?range=1h");
      await expect(page.getByRole("link", { name: "payments-api" }).first()).toBeVisible();
    } finally {
      await context.close();
    }
  });

  test("Quickwit 중단: 로그 섹션만 강등되고 Event는 남는다", async ({ browser }) => {
    const { context, page } = await authedPage(browser, FIXTURE.quickwitOutage, "operator", ["platform.admin"]);
    try {
	  const responsePromise = page.waitForResponse((response) => response.url().includes("/api/v1/clusters/prod-seoul/logs?") && response.status() === 200);
	  await page.goto("/logs?ns=payments&range=1h");
	  const payload = (await (await responsePromise).json()) as {
		lines: { status: string; source: string };
		events: { status: string; data: Array<{ involved: { podUid: string } }> };
	  };
	  expect(payload.lines).toMatchObject({ status: "degraded", source: "quickwit" });
	  expect(payload.events.status).toBe("ok");
	  expect(payload.events.data.some((event) => event.involved.podUid === "pod-payments-api-7f-bbb")).toBe(true);
      await expect(page.getByText("Quickwit 응답 없음").first()).toBeVisible();
      /* 다른 화면(Overview)의 메트릭은 영향이 없습니다. */
      await page.goto("/?range=1h");
      await expect(page.getByText("CPU 사용률").first()).toBeVisible();
      await expect(page.getByText("GreptimeDB 응답 없음")).toHaveCount(0);
    } finally {
      await context.close();
    }
  });

  test("Alert backend 중단: 알림 섹션만 강등된다", async ({ browser }) => {
    const { context, page } = await authedPage(browser, FIXTURE.alertsOutage, "operator", ["platform.admin"]);
    try {
      await page.goto("/?range=1h");
      await expect(page.getByText("Alertmanager 응답 없음").first()).toBeVisible();
      /* 메트릭·이상 엔티티는 그대로입니다. */
      await expect(page.getByText("CPU 사용률").first()).toBeVisible();
      await expect(page.getByRole("link", { name: "payments-api-7f-bbb" }).first()).toBeVisible();
      /* 알림 전용 화면도 죽지 않고 강등 사유를 보여줍니다. */
      await page.goto("/alerts?range=1h");
      await expect(page.getByText("Alertmanager 응답 없음").first()).toBeVisible();
    } finally {
      await context.close();
    }
  });
});
