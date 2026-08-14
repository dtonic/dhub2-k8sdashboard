import { expect, test, type Page, type Response } from "@playwright/test";
import { FIXTURE } from "../playwright.integration.config";
import { authedPage, clickBudget, trackApi, trackFailures } from "./helpers";

/**
 * 통합 여정 검증 (#22) — 프로덕션 번들 + 실제 Go BFF.
 *
 * 클릭 예산: 릴리스 기준은 **Overview에서 원인 Pod와 관련 로그까지 4클릭 이내**입니다.
 * 여기서 "클릭"은 화면 전환을 일으키는 의미 있는 조작 1회이며, 최초 진입(goto)은
 * 클릭이 아닙니다. 실제 여정은 2클릭(이상 엔티티 → Pod 상세 → 로그)입니다.
 */

function apiResponse(page: Page, part: string): Promise<Response> {
  return page.waitForResponse((r) => r.url().includes("/api/v1/") && r.url().includes(part));
}

test.describe("통합 여정 — 장애 발견부터 원인 로그까지", () => {
  test("Overview → 원인 Pod → 관련 로그: 4클릭 이내 · UID/ns/시간창 보존 · N+1 없음", async ({ browser }) => {
    const { context, page } = await authedPage(browser, FIXTURE.main, "oncall-admin", ["platform.admin"]);
    try {
      const api = trackApi(page);
      const failures = trackFailures(page);
      const budget = clickBudget();

      const overviewRes = apiResponse(page, "/overview");
      await page.goto("/");
      await overviewRes;

      // 프로덕션 번들 증거: MSW 서비스워커가 없고, 워커 요청도 없습니다.
      const swCount = await page.evaluate(async () => (await navigator.serviceWorker.getRegistrations()).length);
      expect(swCount).toBe(0);

      // 화면 하나 = 요청 하나. Overview는 /scope + /overview 두 번을 넘지 않습니다.
      await expect(page.getByRole("link", { name: "payments-api-7f-bbb" }).first()).toBeVisible();
      expect(api.matching(/\/overview/)).toHaveLength(1);
      expect(api.count()).toBeLessThanOrEqual(2);

      // 클릭 1 — 이상 엔티티 Top N에서 원인 Pod로.
      const podRes = apiResponse(page, "/pods/payments-api-7f-bbb");
      await budget.click(page.getByRole("link", { name: "payments-api-7f-bbb" }).first());
      const podJson = (await (await podRes).json()) as {
        range: { from: string; to: string };
      };
      const podURL = new URL(page.url());
      expect(podURL.pathname).toBe("/pods/payments-api-7f-bbb");
      expect(podURL.searchParams.get("uid")).toBe("pod-payments-api-7f-bbb");
      expect(podURL.searchParams.get("ns")).toBe("payments");
      // Event 상관: 시나리오 Warning 이벤트가 같은 화면에 있습니다.
      await expect(page.getByText("Back-off restarting failed container").first()).toBeVisible();
      expect(api.matching(/\/pods\/payments-api-7f-bbb/)).toHaveLength(1);

      // 클릭 2 — 같은 시간창으로 관련 로그.
      const logsRes = apiResponse(page, "/logs?");
      await budget.click(page.getByRole("link", { name: /이 Pod의 로그 열기/ }));
      const logsJson = (await (await logsRes).json()) as {
        applied: { from: string; to: string };
		lines: { data: Array<{ id: string; podUid: string }> };
      };
      const logsURL = new URL(page.url());
      expect(logsURL.pathname).toBe("/logs");
      expect(logsURL.searchParams.get("uid")).toBe("pod-payments-api-7f-bbb");
      expect(logsURL.searchParams.get("ns")).toBe("payments");
      // 시간창 보존: 서버가 확정한 조회 구간이 Pod 상세와 정확히 같습니다.
      expect(logsJson.applied.from).toBe(podJson.range.from);
      expect(logsJson.applied.to).toBe(podJson.range.to);
	  // 응답과 가상화된 UI 첫 화면 모두 원인 Pod 로그를 표시합니다.
	  expect(logsJson.lines.data.some((line) => line.id.startsWith("e2e-crashloop-") && line.podUid === "pod-payments-api-7f-bbb")).toBe(true);
	  await expect(page.locator(".logline").first()).toBeVisible();
      const logCalls = api.matching(/\/logs\?/);
      expect(logCalls.length).toBeGreaterThanOrEqual(1);
      expect(logCalls[0]).toContain("podUid=pod-payments-api-7f-bbb");

      // 릴리스 기준: 4클릭 이내. 실측을 그대로 단언합니다.
	  expect(budget.total()).toBe(2);
	  expect(api.count()).toBeLessThanOrEqual(4);
      expect(failures).toEqual([]);
      // 토큰은 헤더로만 다닙니다 — URL에 토큰 모양 값이 없습니다.
      expect(api.all().filter((c) => /access_token|eyJ/.test(c))).toEqual([]);
    } finally {
      await context.close();
    }
  });

  test("Namespace → Workload → Pod 드릴다운: 세대 표시와 UID 링크, 화면당 요청 1건", async ({ browser }) => {
    const { context, page } = await authedPage(browser, FIXTURE.main, "oncall-admin", ["platform.admin"]);
    try {
      const api = trackApi(page);
      const budget = clickBudget();

      const nsRes = apiResponse(page, "/namespaces/payments");
      await page.goto("/namespaces/payments");
      await nsRes;
      expect(api.matching(/\/namespaces\/payments\?/)).toHaveLength(1);

      // Workload 목록 → payments-api 상세.
      const wlRes = apiResponse(page, "/workloads/Deployment/payments-api");
      await budget.click(page.getByRole("link", { name: "payments-api", exact: true }).first());
      await wlRes;
      expect(api.matching(/\/workloads\/Deployment\/payments-api/)).toHaveLength(1);
      // 소유 체인(Deployment → ReplicaSet 세대)이 그려집니다.
      await expect(page.locator(".owner-chain__item").first()).toBeVisible();

      // Pod 목록의 링크는 이름이 아니라 UID로 식별합니다.
      const podLink = page.getByRole("link", { name: "payments-api-7f-bbb" }).first();
      await expect(podLink).toBeVisible();
      expect(await podLink.getAttribute("href")).toContain("uid=pod-payments-api-7f-bbb");

      const podRes = apiResponse(page, "/pods/payments-api-7f-bbb");
      await budget.click(podLink);
      await podRes;
      await expect(page.locator("h1")).toContainText("payments-api-7f-bbb");
      expect(api.matching(/\/pods\/payments-api-7f-bbb/)).toHaveLength(1);
      expect(budget.total()).toBeLessThanOrEqual(4);
    } finally {
      await context.close();
    }
  });

  test("네 시나리오 corpus가 화면 증거를 남긴다 (CrashLoop·ImagePull·CPU spike·Error log)", async ({ browser }) => {
    const { context, page } = await authedPage(browser, FIXTURE.main, "oncall-admin", ["platform.admin"]);
    try {
      // 1·2. CrashLoopBackOff와 ImagePullBackOff는 Overview 이상 엔티티에 뜹니다.
      const overviewRes = apiResponse(page, "/overview");
      await page.goto("/");
      await overviewRes;
      await expect(page.getByRole("link", { name: "payments-api-7f-bbb" }).first()).toBeVisible();
      await expect(page.getByRole("link", { name: "media-api-1a-eee" }).first()).toBeVisible();
      await expect(page.getByText("CrashLoopBackOff").first()).toBeVisible();
      await expect(page.getByText("ImagePullBackOff").first()).toBeVisible();

      // 3. CPU spike: Pod 상세의 CPU used 계열이 스파이크 구간에서 90 이상입니다.
      const batchRes = apiResponse(page, "/pods/batch-sync-qq81z");
      await page.goto("/pods/batch-sync-qq81z?ns=payments&uid=pod-batch-sync-qq81z");
      const batch = (await (await batchRes).json()) as {
        trends: { data: Array<{ id: string; series: Array<{ key: string; points: Array<{ t: number; v: number }> }> }> };
      };
      const used = batch.trends.data.find((p) => p.id === "cpu")?.series.find((s) => s.key === "used");
      expect(used).toBeTruthy();
      const points = used!.points;
      expect(points.length).toBeGreaterThan(0);
      expect(points[points.length - 1]!.v).toBeGreaterThanOrEqual(90);
      expect(points[0]!.v).toBeLessThan(50);
      // 상관 이벤트(CPU 포화 → liveness 시간 초과)가 같은 화면에 있습니다.
      await expect(page.getByText("Liveness probe failed: context deadline exceeded").first()).toBeVisible();

      // 4. Error log: 루트 Pod로 좁힌 Logs Explorer에 시나리오 ERROR가 보입니다.
	  const logsRes = apiResponse(page, "/logs?");
      await page.goto("/logs?ns=media&uid=pod-media-api-zzz");
	  const errorLogs = (await (await logsRes).json()) as { lines: { data: Array<{ id: string; podUid: string }> } };
	  expect(errorLogs.lines.data.some((line) => line.id.startsWith("e2e-errorlog-") && line.podUid === "pod-media-api-zzz")).toBe(true);
	  await expect(page.locator(".logline").first()).toBeVisible();

		/* 네 시나리오 모두에서 같은 root Pod UID와 같은 1h 창이 metric/event/log/alert를 잇습니다. */
		const scenarios = [
			["crashloop", "payments", "payments-api-7f-bbb", "pod-payments-api-7f-bbb", "BackOff"],
			["imagepull", "media", "media-api-1a-eee", "pod-media-api-1a-eee", "Failed"],
			["cpuspike", "payments", "batch-sync-qq81z", "pod-batch-sync-qq81z", "Unhealthy"],
			["errorlog", "media", "media-api-zzz", "pod-media-api-zzz", "Unhealthy"],
		] as const;
		for (const [id, ns, pod, uid, reason] of scenarios) {
			const podResponse = await context.request.get(`/api/v1/clusters/prod-seoul/pods/${pod}?ns=${ns}&uid=${uid}&range=1h`);
			expect(podResponse.status()).toBe(200);
			const detail = (await podResponse.json()) as {
				range: { from: string; to: string };
				pod: { data: { uid: string } };
				trends: { status: string; data: unknown[] };
				events: { data: Array<{ reason: string; involved: { podUid: string } }> };
			};
			expect(detail.pod.data.uid).toBe(uid);
			expect(detail.trends.status).toBe("ok");
			expect(detail.trends.data.length).toBeGreaterThan(0);
			expect(detail.events.data.some((event) => event.reason === reason && event.involved.podUid === uid)).toBe(true);

			const logResponse = await context.request.get(`/api/v1/clusters/prod-seoul/logs?ns=${ns}&podUid=${uid}&range=1h&levels=ERROR`);
			expect(logResponse.status()).toBe(200);
			const logs = (await logResponse.json()) as {
				applied: { from: string; to: string };
				lines: { data: Array<{ id: string; podUid: string }> };
			};
			expect(logs.applied).toMatchObject({ from: detail.range.from, to: detail.range.to });
			expect(logs.lines.data.some((line) => line.id.startsWith(`e2e-${id}-`) && line.podUid === uid)).toBe(true);

			const alertResponse = await context.request.get(`/api/v1/clusters/prod-seoul/alerts?ns=${ns}&range=1h`);
			expect(alertResponse.status()).toBe(200);
			const alerts = (await alertResponse.json()) as {
				range: { from: string; to: string };
				firing: { data: Array<{ id: string; entity: { podUid: string } }> };
			};
			expect(alerts.range).toEqual(detail.range);
			expect(alerts.firing.data.some((alert) => alert.id === `e2e-alert-${id}` && alert.entity.podUid === uid)).toBe(true);
		}
    } finally {
      await context.close();
    }
  });

  test("Alert 상관: 네 시나리오 알림이 보이고 Entity 딥링크가 UID를 보존한다", async ({ browser }) => {
    const { context, page } = await authedPage(browser, FIXTURE.main, "oncall-admin", ["platform.admin"]);
    try {
      const budget = clickBudget();
      const alertsRes = apiResponse(page, "/alerts?");
      await page.goto("/alerts?alert=e2e-alert-crashloop");
      await alertsRes;

      for (const name of ["PodCrashLooping", "PodImagePullBackOff", "CPUThrottlingHigh", "HighErrorRate"]) {
        await expect(page.getByText(name).first()).toBeVisible();
      }

      // 선택된 알림의 상세에서 관련 대상으로 — UID·ns가 그대로 실립니다.
      const targetLink = page.getByRole("link", { name: /관련 대상 상세/ });
      await expect(targetLink).toBeVisible();
      expect(await targetLink.getAttribute("href")).toContain("uid=pod-payments-api-7f-bbb");

      const podRes = apiResponse(page, "/pods/payments-api-7f-bbb");
      await budget.click(targetLink);
      await podRes;
      const u = new URL(page.url());
      expect(u.pathname).toBe("/pods/payments-api-7f-bbb");
      expect(u.searchParams.get("uid")).toBe("pod-payments-api-7f-bbb");
      expect(budget.total()).toBeLessThanOrEqual(4);
    } finally {
      await context.close();
    }
  });
});
