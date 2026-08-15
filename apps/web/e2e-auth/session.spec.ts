import { execFileSync } from "node:child_process";
import { expect, test, type Page } from "@playwright/test";

const streamPath = "/api/v1/clusters/prod-seoul/events/stream";

test("server-side OIDC session, CSRF, nginx reconnect, replay, and leak boundaries", async ({ page, context, browser, baseURL }) => {
  const requests: Array<{ url: string; headers: Record<string, string>; body: string | null }> = [], consoleLines: string[] = [], responseBodies: string[] = [];
  const responseReads: Promise<void>[] = [], pageSnapshots: string[] = [];
  const attachPage = (target: Page) => {
    target.on("request", (request) => requests.push({ url: request.url(), headers: request.headers(), body: request.postData() }));
    target.on("console", (message) => consoleLines.push(message.text()));
    target.on("response", (response) => { if (!response.url().includes("/events/stream") && !response.url().includes("/assets/")) responseReads.push(response.text().then((body) => { if (body.length <= 65_536) responseBodies.push(body); }).catch(() => undefined)); });
  };
  const snapshotPage = async (target: Page) => pageSnapshots.push(JSON.stringify(await target.evaluate(() => ({ local: { ...localStorage }, session: { ...sessionStorage }, html: document.documentElement.outerHTML }))));
  attachPage(page);
  const cdp = await context.newCDPSession(page);
  const streamRequestIDs = new Set<string>(); let streamWireBytes = 0; const streamResponseHeaders: Record<string, string>[] = [];
  await cdp.send("Network.enable");
  cdp.on("Network.responseReceived", ({ requestId, response }) => {
    if (new URL(response.url).pathname !== streamPath) return;
    streamRequestIDs.add(requestId); streamResponseHeaders.push(Object.fromEntries(Object.entries(response.headers).map(([key, value]) => [key.toLowerCase(), String(value)])));
  });
  cdp.on("Network.dataReceived", ({ requestId, dataLength }) => { if (streamRequestIDs.has(requestId)) streamWireBytes += dataLength; });

  expect((await context.request.get(`${baseURL}/api/v1/scope`)).status()).toBe(401);
  await page.goto("/"); await expect(page.getByRole("heading", { name: "Sign in required" })).toBeVisible();
  await page.getByRole("link", { name: "Sign in" }).click(); await expect(page.getByText("Browser Admin")).toBeVisible();
  expect(page.url()).not.toContain("code=");

  const session = await page.evaluate(async () => (await fetch("/api/v1/auth/session")).json());
  expect(session).toMatchObject({ authenticated: true, principal: { displayName: "Browser Admin" } });
  const afterLoginEvidence = await (await context.request.get(`${baseURL}/e2e/evidence`)).json();
  const csrf = String(session.csrfToken); expect(csrf).toMatch(/^[A-Za-z0-9_-]{43}$/);
  expect((await context.request.post(`${baseURL}/api/v1/auth/refresh`, { headers: { Origin: String(baseURL) } })).status()).toBe(403);
  expect((await context.request.post(`${baseURL}/api/v1/auth/refresh`, { headers: { Origin: "https://evil.example", "X-CSRF-Token": csrf } })).status()).toBe(403);
  expect((await (await context.request.get(`${baseURL}/e2e/evidence`)).json()).refresh).toBe(afterLoginEvidence.refresh);

  const beforeCookie = (await context.cookies()).find((item) => item.name === "__Host-k8s-dashboard")!;
  expect((await context.request.post(`${baseURL}/api/v1/auth/refresh`, { headers: { Origin: String(baseURL), "X-CSRF-Token": csrf } })).status()).toBe(204);
  const afterCookie = (await context.cookies()).find((item) => item.name === "__Host-k8s-dashboard")!;
  expect(afterCookie).toMatchObject({ secure: true, httpOnly: true, sameSite: "Lax", path: "/" }); expect(Math.abs(afterCookie.expires - beforeCookie.expires)).toBeLessThan(2);

  await expect.poll(() => requests.filter((request) => new URL(request.url).pathname === streamPath).length).toBeGreaterThan(0);
  await expect.poll(async () => Number((await (await context.request.get(`${baseURL}/e2e/evidence`)).json()).streamConnections)).toBeGreaterThan(0);
  await page.evaluate(() => {
    document.documentElement.dataset.e2eStreamMessages = "0";
    window.addEventListener("dashboard-stream-message", () => {
      document.documentElement.dataset.e2eStreamMessages = String(Number(document.documentElement.dataset.e2eStreamMessages ?? "0") + 1);
    });
  });
  const deliveriesBeforeRestart = Number((await (await context.request.get(`${baseURL}/e2e/evidence`)).json()).streamDeliveries);
  await context.request.post(`${baseURL}/e2e/publish`);
  await expect.poll(async () => Number((await (await context.request.get(`${baseURL}/e2e/evidence`)).json()).streamDeliveries)).toBe(deliveriesBeforeRestart + 1);
  try { await expect.poll(() => page.evaluate(() => Number(document.documentElement.dataset.e2eStreamMessages ?? "0"))).toBe(1); } catch {
    throw new Error(`SSE parser did not observe delivered bytes ${JSON.stringify({ streamWireBytes, headers: streamResponseHeaders.map((headers) => ({ contentType: headers["content-type"], cacheControl: headers["cache-control"], buffering: headers["x-accel-buffering"], encoding: headers["content-encoding"] ?? "" })) })}`);
  }
  expect(streamWireBytes).toBeGreaterThan(0);
  expect(streamResponseHeaders.some((headers) => headers["content-type"] === "text/event-stream; charset=utf-8" && headers["cache-control"] === "no-store" && headers["x-accel-buffering"] === "no" && !headers["content-encoding"])).toBe(true);
  const ownedContainer = process.env.AUTH_NGINX_CONTAINER; expect(ownedContainer).toBe("dashboard-issue1-nginx-auth-20260815");
  execFileSync("docker", ["restart", ownedContainer!], { stdio: "pipe" });
  await expect.poll(() => requests.filter((request) => new URL(request.url).pathname === streamPath && Boolean(request.headers["last-event-id"])).length, { timeout: 15_000 }).toBeGreaterThan(0);
  await context.request.post(`${baseURL}/e2e/publish`); await page.waitForTimeout(400);

  const readOne = async (lastEventID = "") => page.evaluate(async ({ path, last }) => {
    const controller = new AbortController(); const headers: Record<string,string> = { Accept: "text/event-stream" }; if (last) headers["Last-Event-ID"] = last;
    const response = await fetch(path, { headers, signal: controller.signal }); const reader = response.body!.getReader(); let text = "";
    try { while (!/^id:/m.test(text)) { const { done, value } = await reader.read(); if (done) break; text += new TextDecoder().decode(value); } } finally { controller.abort(); try { await reader.cancel(); } catch {} reader.releaseLock(); }
    const ids = [...text.matchAll(/^id: ([^\r\n]+)/gm)].map((match) => match[1]!);
    return { text, id: ids.at(-1) ?? "" };
  }, { path: streamPath, last: lastEventID });
  const connectionsBeforeManual = Number((await (await context.request.get(`${baseURL}/e2e/evidence`)).json()).streamConnections);
  const firstPending = readOne(); await expect.poll(async () => Number((await (await context.request.get(`${baseURL}/e2e/evidence`)).json()).streamConnections)).toBeGreaterThan(connectionsBeforeManual); await context.request.post(`${baseURL}/e2e/publish`);
  const first = await firstPending; expect(first.id).not.toBe(""); await context.request.post(`${baseURL}/e2e/publish`); const replay = await readOne(first.id); expect(replay.id).not.toBe(first.id); expect(replay.text.match(/^id:/gm)).toHaveLength(1);

  const evidence = await (await context.request.get(`${baseURL}/e2e/evidence`)).json(); expect(evidence.authorize).toBe(afterLoginEvidence.authorize); expect(evidence.token).toBe(afterLoginEvidence.token); expect(evidence.refresh).toBe(afterLoginEvidence.refresh + 1); expect(evidence.replica0).toBeGreaterThan(0); expect(evidence.replica1).toBeGreaterThan(0);
  await page.getByRole("button", { name: "Sign out" }).click(); await expect(page.getByRole("heading", { name: "Sign in required" })).toBeVisible();
  await expect.poll(async () => (await (await context.request.get(`${baseURL}/e2e/evidence`)).json()).streamConnections).toBe(0);
  expect((await (await context.request.get(`${baseURL}/api/v1/auth/session`)).json()).authenticated).toBe(false);

  expect((await context.request.post(`${baseURL}/e2e/token-ttl?value=4s`)).status()).toBe(204); expect((await context.request.post(`${baseURL}/e2e/refresh-delay?value=2500ms`)).status()).toBe(204);
  const multi = await browser.newContext({ baseURL, ignoreHTTPSErrors: true }); const tabA = await multi.newPage(); attachPage(tabA); await tabA.goto("/"); await tabA.getByRole("link", { name: "Sign in" }).click(); await expect(tabA.getByText("Browser Admin")).toBeVisible();
  const tabB = await multi.newPage(); attachPage(tabB); await tabB.goto("/"); await expect(tabB.getByText("Browser Admin")).toBeVisible(); const beforeConcurrent = Number((await (await multi.request.get(`${baseURL}/e2e/evidence`)).json()).refresh);
  await expect.poll(async () => Number((await (await multi.request.get(`${baseURL}/e2e/evidence`)).json()).refresh), { timeout: 10_000 }).toBe(beforeConcurrent + 1);
  await expect(tabA.getByText("Browser Admin")).toBeVisible(); await expect(tabB.getByText("Browser Admin")).toBeVisible(); expect(await tabA.getByRole("heading", { name: "Authentication unavailable" }).count()).toBe(0); expect(await tabB.getByRole("heading", { name: "Authentication unavailable" }).count()).toBe(0); expect((await (await multi.request.get(`${baseURL}/api/v1/auth/session`)).json()).authenticated).toBe(true); await snapshotPage(tabA); await snapshotPage(tabB); await multi.close();

  // A closed/suspended tab cannot run the proactive timer. The opaque cookie
  // and Redis record remain usable before idle/absolute expiry, while expired
  // claims authorize nothing until the server-only refresh succeeds.
  expect((await context.request.post(`${baseURL}/e2e/token-ttl?value=6s`)).status()).toBe(204);
  const wake = await browser.newContext({ baseURL, ignoreHTTPSErrors: true });
  await wake.addInitScript(() => {
    const state = window as typeof window & { __authLifecycle?: string[] };
    state.__authLifecycle = [];
    window.addEventListener("dashboard-session-refreshed", () => state.__authLifecycle?.push("refreshed"));
    window.addEventListener("dashboard-session-expired", () => state.__authLifecycle?.push("expired"));
  });
  const wakePage = await wake.newPage();
  attachPage(wakePage);
  await wakePage.goto("/"); await wakePage.getByRole("link", { name: "Sign in" }).click(); await expect(wakePage.getByText("Browser Admin")).toBeVisible();
  const wakeSession = await wakePage.evaluate(async () => (await fetch("/api/v1/auth/session")).json()); expect(wakeSession.authenticated).toBe(true); await snapshotPage(wakePage); await wakePage.close();
  expect((await wake.request.post(`${baseURL}/e2e/next-role?value=viewer`)).status()).toBe(204);
  await new Promise((resolve) => setTimeout(resolve, 6_500));
  expect((await wake.request.get(`${baseURL}/api/v1/scope`)).status()).toBe(401);
  const beforeWakeRefresh = Number((await (await wake.request.get(`${baseURL}/e2e/evidence`)).json()).refresh);
  const recovered = await wake.newPage(); attachPage(recovered);
  await recovered.goto("/");
  try { await expect(recovered.getByText("Browser Admin")).toBeVisible(); } catch {
    const diagnostic = await recovered.evaluate(() => ({ events: (window as typeof window & { __authLifecycle?: string[] }).__authLifecycle ?? [], heading: document.querySelector("h1")?.textContent ?? "none", text: (document.body.innerText || "").slice(0, 160) }));
    throw new Error(`wake auth lifecycle ${JSON.stringify(diagnostic)}`);
  }
  expect(await recovered.getByRole("heading", { name: /Sign in required|Authentication unavailable/ }).count()).toBe(0);
  expect(Number((await (await wake.request.get(`${baseURL}/e2e/evidence`)).json()).refresh)).toBe(beforeWakeRefresh + 1); const recoveredSession=await (await wake.request.get(`${baseURL}/api/v1/auth/session`)).json(); expect(recoveredSession).toMatchObject({authenticated:true,capabilities:{canEditDashboard:false,canPublishDashboard:false}});
  await recovered.getByRole("button",{name:"Sign out"}).click(); await expect(recovered.getByRole("heading",{name:"Sign in required"})).toBeVisible(); expect((await (await wake.request.get(`${baseURL}/api/v1/auth/session`)).json()).authenticated).toBe(false);
  await snapshotPage(recovered); await recovered.close(); await wake.close();

  await snapshotPage(page); await Promise.all(responseReads);
  const callbacks = requests.filter((request) => new URL(request.url).pathname === "/api/v1/auth/callback"); expect(callbacks).toHaveLength(3); for (const callback of callbacks) expect(new URL(callback.url).searchParams.get("code")).toMatch(/^[A-Za-z0-9_-]{43}$/);
  for (const request of requests) { const url = new URL(request.url); if (url.pathname !== "/api/v1/auth/callback") expect(url.searchParams.has("code")).toBe(false); const sensitive=/access-|refresh_token|id_token/i.test(`${request.url}\n${JSON.stringify(request.headers)}\n${request.body ?? ""}`); expect(sensitive,"OIDC token leaked into a browser-visible request").toBe(false); }
  const sensitiveArtifact=/access-|refresh_token|id_token|browser-admin/i.test(JSON.stringify({ pageSnapshots, consoleLines, responseBodies }));
  expect(sensitiveArtifact,"OIDC token or raw subject leaked into browser-visible state").toBe(false);
});
