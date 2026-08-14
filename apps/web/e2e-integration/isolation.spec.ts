import { expect, request, test } from "@playwright/test";
import { FIXTURE } from "../playwright.integration.config";
import { authedPage, trackFailures } from "./helpers";

/**
 * 권한 격리 (#22): 실제 mock OIDC/JWKS 검증 경로 위에서, 역할이 다른 두 브라우저
 * 컨텍스트가 서로의 데이터를 보지 못하는지 검증합니다. Scope는 서버가 강제합니다.
 */
test.describe("권한별 데이터 격리 (#22)", () => {
  test("무인증 요청은 401로 끝난다", async () => {
    const api = await request.newContext({ baseURL: FIXTURE.main });
    try {
      const res = await api.get("/api/v1/scope");
      expect(res.status()).toBe(401);
    } finally {
      await api.dispose();
    }
  });

  test("payments/media 두 사용자는 같은 URL과 cache에서도 서로의 namespace를 보지 못한다", async ({ browser }) => {
	const payments = await authedPage(browser, FIXTURE.main, "payments-viewer", ["namespace.viewer:payments"]);
	const viewer = await authedPage(browser, FIXTURE.main, "media-viewer", ["namespace.viewer:media"]);
    try {
		await payments.page.goto("/?range=1h");
		await expect(payments.page.getByRole("link", { name: "payments-api-7f-bbb" }).first()).toBeVisible();
		await expect(payments.page.getByRole("link", { name: "media-api-1a-eee" })).toHaveCount(0);

      await viewer.page.goto("/?range=1h");
      /* 허용 범위(media)의 이상 엔티티는 보입니다. */
      await expect(viewer.page.getByRole("link", { name: "media-api-1a-eee" }).first()).toBeVisible();
      /* payments의 데이터는 링크 하나도 나가면 안 됩니다. */
      await expect(viewer.page.getByRole("link", { name: "payments-api-7f-bbb" })).toHaveCount(0);
      await expect(viewer.page.getByText("batch-sync-qq81z")).toHaveCount(0);
		/* 동일 API URL을 두 bearer context에서 읽어도 scope별 cache가 섞이지 않습니다. */
		const apiPath = "/api/v1/clusters/prod-seoul/overview?range=1h";
		const [paymentsResponse, mediaResponse] = await Promise.all([
			payments.context.request.get(apiPath), viewer.context.request.get(apiPath),
		]);
		expect(paymentsResponse.status()).toBe(200);
		expect(mediaResponse.status()).toBe(200);
		const paymentsBody = JSON.stringify(await paymentsResponse.json());
		const mediaBody = JSON.stringify(await mediaResponse.json());
		expect(paymentsBody).toContain("pod-payments-api-7f-bbb");
		expect(paymentsBody).not.toContain("pod-media-api-1a-eee");
		expect(mediaBody).toContain("pod-media-api-1a-eee");
		expect(mediaBody).not.toContain("pod-payments-api-7f-bbb");
    } finally {
		await payments.context.close();
      await viewer.context.close();
    }
  });

  test("범위 밖 딥링크는 데이터 없이 거절된다", async ({ browser }) => {
    const viewer = await authedPage(browser, FIXTURE.main, "media-viewer", ["namespace.viewer:media"]);
    try {
      const failures = trackFailures(viewer.page);

      /* Namespace 딥링크 — 403 안내가 뜨고 목록은 부분적으로도 그려지지 않습니다. */
      await viewer.page.goto("/namespaces/payments?range=1h");
      await expect(viewer.page.getByText("권한이 없습니다").first()).toBeVisible();
      await expect(viewer.page.getByText("payments-api-7f-bbb")).toHaveCount(0);

      /* Pod 딥링크 — URL을 직접 만들어도 서버 Scope가 막습니다. */
      await viewer.page.goto("/pods/payments-api-7f-bbb?ns=payments&uid=pod-payments-api-7f-bbb&range=1h");
      await expect(viewer.page.getByText("권한이 없습니다").first()).toBeVisible();
      await expect(viewer.page.getByText(/Back-off restarting/)).toHaveCount(0);

      /* 거절은 403(권한 부족)이지 401(인증 실패)이나 빈 200이 아닙니다. */
      expect(failures.some((f) => f.startsWith("403 "))).toBe(true);
      expect(failures.some((f) => f.startsWith("401 "))).toBe(false);

      /* 토큰은 URL에 실리지 않습니다 — Bearer 헤더만 씁니다. */
      expect(viewer.page.url()).not.toMatch(/eyJ|token=/);
    } finally {
      await viewer.context.close();
    }
  });

  test("역할이 없는 유효 토큰은 401이 아니라 403(빈 Scope)이다", async () => {
    const api = await request.newContext({ baseURL: FIXTURE.main });
    try {
      const tokenRes = await api.post("/e2e/token", { data: { sub: "no-roles", roles: [] } });
      const { access_token } = (await tokenRes.json()) as { access_token: string };
      const res = await api.get("/api/v1/clusters/prod-seoul/overview?range=1h", {
        headers: { authorization: `Bearer ${access_token}` },
      });
      expect(res.status()).toBe(403);
    } finally {
      await api.dispose();
    }
  });
});
