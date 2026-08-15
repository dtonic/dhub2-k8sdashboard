import { expect, test, type Browser, type Page } from "@playwright/test";

async function login(page: Page) {
  let lastError: unknown;
  for (let attempt = 0; attempt < 20; attempt += 1) {
    try {
      await page.goto("/");
      lastError = undefined;
      break;
    } catch (error) {
      if (!String(error).includes("ERR_CONNECTION_REFUSED")) throw error;
      lastError = error;
      await page.waitForTimeout(100);
    }
  }
  if (lastError) throw lastError;
  await page.getByRole("link", { name: "Sign in" }).click();
  await expect(page.getByText("Browser Admin")).toBeVisible();
  await expect(page.getByText("CPU 사용률").first()).toBeVisible();
}

function observe(page: Page) {
  const requests: Array<{ url: string; method: string; headers: Record<string, string>; body: string | null }> = [];
  const consoleLines: string[] = [];
  const responseBodies: string[] = [];
  const reads: Promise<void>[] = [];
  page.on("request", (request) => requests.push({ url: request.url(), method: request.method(), headers: request.headers(), body: request.postData() }));
  page.on("console", (message) => consoleLines.push(message.text()));
  page.on("response", (response) => {
    const contentType = response.headers()["content-type"] ?? "";
    if (response.url().includes("/assets/") || (new URL(response.url()).pathname === "/api/v1/clusters/prod-seoul/events/stream" && contentType.startsWith("text/event-stream"))) return;
    reads.push(response.text().then((body) => { if (body.length <= 65_536) responseBodies.push(body); }).catch(() => undefined));
  });
  return { requests, consoleLines, responseBodies, reads };
}

function isAlertsAPI(rawURL: string) {
  return new URL(rawURL).pathname === "/api/v1/clusters/prod-seoul/alerts";
}

async function assertNoLeak(page: Page, evidence: ReturnType<typeof observe>) {
  await Promise.all(evidence.reads);
  const browserState = await page.evaluate(() => JSON.stringify({ html: document.documentElement.outerHTML, local: { ...localStorage }, session: { ...sessionStorage } }));
  const serialized = JSON.stringify({ browserState, console: evidence.consoleLines, responses: evidence.responseBodies, requests: evidence.requests });
  for (const sentinel of ["raw_secret_label", "raw-label-must-not-leak", "generator.invalid", "must-not-leak"]) {
    expect(serialized, `${sentinel} leaked to browser-visible state`).not.toContain(sentinel);
  }
  expect(serialized.includes(process.env.ALERTMANAGER_BROWSER_TOKEN!), "bearer token leaked to browser-visible state").toBe(false);
  expect(evidence.requests.some((request) => Object.keys(request.headers).some((name) => name.toLowerCase() === "authorization"))).toBe(false);
}

async function newSession(browser: Browser, baseURL: string) {
  const context = await browser.newContext({ baseURL, ignoreHTTPSErrors: true });
  const page = await context.newPage();
  await login(page);
  return { context, page };
}

test("live firing drill-down, independent history degradation, read-only controls, and leak boundary", async ({ browser, baseURL }) => {
  const { context, page } = await newSession(browser, String(baseURL));
  const evidence = observe(page);
  try {
    const marker = evidence.requests.length;
    const responsePromise = page.waitForResponse((response) => isAlertsAPI(response.url()) && response.status() === 200);
    await page.goto("/alerts?ns=payments&range=1h");
    const response = await responsePromise;
    const payload = await response.json() as {
      firing: { status: string; data: Array<{ id: string; entity: { podUid: string; workloadUid: string; workloadName: string }; sourceUrl: string }> };
      resolved: { status: string; reason: string };
      counts: { status: string; reason: string };
    };
    expect(payload.firing.status, JSON.stringify(payload.firing)).toBe("ok");
    expect(payload.firing.data).toHaveLength(1);
    expect(payload.firing.data[0]!.entity).toMatchObject({ podUid: "pod-payments-api-7f-bbb", workloadUid: "dep-payments-api", workloadName: "payments-api" });
    expect(payload.resolved).toMatchObject({ status: "degraded", reason: "history_not_configured" });
    expect(payload.counts).toMatchObject({ status: "degraded", reason: "history_not_configured" });
    const viewRequests = evidence.requests.slice(marker).filter((request) => isAlertsAPI(request.url));
    expect(viewRequests).toHaveLength(1);
    expect(viewRequests[0]!.method).toBe("GET");

    await expect(page.getByText("LivePodCrashLooping").first()).toBeVisible();
    await expect(page.getByText("Production adapter live firing fixture")).toBeVisible();
    await expect(page.getByText("해소 이력 미구성").first()).toBeVisible();
    await expect(page.getByText("해소 이력 저장소가 구성되지 않아 현재 진행 중인 알림만 표시합니다.").first()).toBeVisible();
    await expect(page.getByRole("tab", { name: /해소됨/ }).locator(".chip__count")).toHaveText("—");
    const target = page.getByRole("link", { name: /관련 대상 상세/ });
    const workload = page.getByRole("link", { name: /관련 Workload 상세/ });
    const logs = page.getByRole("link", { name: /관련 로그/ });
    const source = page.getByRole("link", { name: /Alertmanager 원본 열기/ });
    expect(await target.getAttribute("href")).toContain("/pods/payments-api-7f-bbb");
    expect(await target.getAttribute("href")).toContain("ns=payments");
    expect(await target.getAttribute("href")).toContain("uid=pod-payments-api-7f-bbb");
    expect(await workload.getAttribute("href")).toContain("/workloads/Deployment/payments-api");
    expect(await workload.getAttribute("href")).toContain("ns=payments");
    const workloadHref = await workload.getAttribute("href");
    const logsHref = await logs.getAttribute("href");
    expect(logsHref).toContain("uid=pod-payments-api-7f-bbb");
    const sourceHref = await source.getAttribute("href");
    expect(sourceHref).toContain("https://alerts.public.test/am/#/alerts?");
    expect(sourceHref).toContain("filter=");
    expect(await source.getAttribute("target")).toBe("_blank");
    expect(await source.getAttribute("rel")).toBe("noopener noreferrer");

    await page.getByRole("tab", { name: /해소됨/ }).click();
    await expect(page.getByText("해소 이력 저장소가 구성되지 않아 현재 진행 중인 알림만 표시합니다.").first()).toBeVisible();
    expect(evidence.requests.slice(marker).filter((request) => isAlertsAPI(request.url))).toHaveLength(1);
    await page.getByRole("tab", { name: /진행 중/ }).click();

    await expect(page.locator("button, a").filter({ hasText: /Rule 편집|Silence|Notification Routing/ })).toHaveCount(0);
    expect(evidence.requests.filter((request) => request.method !== "GET" && /alerts|silences|routing|rules/.test(request.url))).toEqual([]);

    const podResponse = page.waitForResponse((response) => response.url().includes("/pods/payments-api-7f-bbb"));
    await target.click();
    await podResponse;
    expect(new URL(page.url()).pathname).toBe("/pods/payments-api-7f-bbb");
    await expect(page.locator("h1")).toContainText("payments-api-7f-bbb");
    const workloadResponse = page.waitForResponse((response) => response.url().includes("/workloads/Deployment/payments-api"));
    await page.goto(workloadHref!);
    await workloadResponse;
    expect(new URL(page.url()).pathname).toBe("/workloads/Deployment/payments-api");
    await expect(page.locator("h1")).toContainText("payments-api");
    const logResponse = page.waitForResponse((response) => new URL(response.url()).pathname.endsWith("/logs"));
    await page.goto(logsHref!);
    await logResponse;
    expect(new URL(page.url()).searchParams.get("uid")).toBe("pod-payments-api-7f-bbb");
    await expect(page.locator(".logline").first()).toBeVisible();

    await assertNoLeak(page, evidence);
  } finally {
    await context.close();
  }
});

test("live Alertmanager outage is isolated from metrics, entities, and the page shell", async ({ browser }) => {
  const outageURL = process.env.ALERTMANAGER_BROWSER_OUTAGE_URL!;
  const { context, page } = await newSession(browser, outageURL);
  const evidence = observe(page);
  try {
    const marker = evidence.requests.length;
    await page.goto("/alerts?ns=payments&range=1h");
    await expect(page.getByText("Alertmanager 응답 없음").first()).toBeVisible();
    expect(evidence.requests.slice(marker).filter((request) => isAlertsAPI(request.url))).toHaveLength(1);
    const overviewMarker = evidence.requests.length;
    await page.goto("/?ns=media&range=6h");
    await expect(page.getByText("CPU 사용률").first()).toBeVisible();
    await expect(page.getByRole("link", { name: "media-api-1a-eee" }).first()).toBeVisible();
    await expect(page.getByText("Alertmanager 응답 없음").first()).toBeVisible();
    expect(evidence.requests.slice(overviewMarker).filter((request) => new URL(request.url).pathname.endsWith("/overview"))).toHaveLength(1);
    const fastFailMarker = evidence.requests.length;
    await page.goto("/alerts?ns=kube-system&range=24h");
    await expect(page.getByText("Alertmanager 응답 없음").first()).toBeVisible();
    expect(evidence.requests.slice(fastFailMarker).filter((request) => isAlertsAPI(request.url))).toHaveLength(1);
    await assertNoLeak(page, evidence);
  } finally {
    await context.close();
  }
});
