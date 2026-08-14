import { createHash } from "node:crypto";
import { expect, test, type Page } from "@playwright/test";
import AxeBuilder from "@axe-core/playwright";

type Call = { method: string; path: string };

function trackBuilder(page: Page): Call[] {
  const calls: Call[] = [];
  page.on("request", (request) => {
    const url = new URL(request.url());
    if (url.pathname.startsWith("/api/")) calls.push({ method: request.method(), path: url.pathname });
  });
  return calls;
}

async function reset(page: Page, scenario = "builder-editor") {
  await page.goto(`/dashboard-builder?scenario=${scenario}&refresh=0`);
  await page.waitForLoadState("networkidle");
  await page.evaluate(async () => {
    const response = await fetch("/api/v1/dashboard-test/reset", { method: "POST" });
    if (!response.ok) throw new Error(`reset failed: ${response.status}`);
  });
  await page.reload();
  await expect(page.getByRole("heading", { name: "Dashboard Builder" })).toBeVisible();
}

test.describe.serial("Dashboard Builder (#24)", () => {
  test("editor clones through both flows and edits with bounded pointer writes", async ({ page }) => {
    const calls = trackBuilder(page);
    await reset(page);

    await page.getByRole("button", { name: /Clone standard Cluster Operations/ }).click();
    await expect(page).toHaveURL(/dashboard-builder\/11111111-/);
    await expect(page.getByRole("heading", { name: "Cluster Operations" }).first()).toBeVisible();
    await page.getByRole("link", { name: "Back to drafts" }).click();
    const cloneResponse = page.waitForResponse((response) => response.request().method() === "POST" && new URL(response.url()).pathname.endsWith("/clone"));
    await page.getByRole("button", { name: "Clone", exact: true }).first().click();
    expect((await cloneResponse).status()).toBe(200);
    await expect(page).toHaveURL(/dashboard-builder\/22222222-/);
    expect(calls.filter((call) => call.method === "POST" && call.path.endsWith("/clone"))).toHaveLength(1);

    await page.getByRole("link", { name: "Back to drafts" }).click();
    await page.getByRole("button", { name: "New draft" }).click();
    await expect(page.getByRole("heading", { name: "Custom dashboard" }).first()).toBeVisible();
    const overviewAtLoad = calls.filter((call) => call.path.includes("/overview")).length;
    expect(overviewAtLoad).toBeGreaterThan(0);

    const putsBeforeLocalEdits = calls.filter((call) => call.method === "PUT").length;
    await page.getByRole("button", { name: "Add Events" }).click();
    const eventsToolbar = page.getByRole("toolbar", { name: "Edit Events" });
    await eventsToolbar.getByRole("button", { name: "Move Events right" }).click();
    await eventsToolbar.getByRole("button", { name: "Make Events taller" }).click();
    await page.getByRole("toolbar", { name: "Edit Nodes Ready" }).getByRole("button", { name: "Delete" }).click();
    expect(calls.filter((call) => call.method === "PUT")).toHaveLength(putsBeforeLocalEdits);
    await page.getByRole("button", { name: "Save changes" }).click();
    await expect(page.getByText(/revision 2/).first()).toBeVisible();
    expect(calls.filter((call) => call.method === "PUT")).toHaveLength(putsBeforeLocalEdits + 1);

    const header = page.locator(".builder-widget .panel__header", { hasText: "Events" });
    const box = await header.boundingBox();
    if (!box) throw new Error("Events header has no bounding box");
    const writesAfterSave = calls.filter((call) => call.method === "PUT").length;
    await page.mouse.move(box.x + 20, box.y + 15);
    await page.mouse.down();
    await page.mouse.up();
    expect(calls.filter((call) => call.method === "PUT")).toHaveLength(writesAfterSave);

    await page.mouse.move(box.x + 20, box.y + 15);
    await page.mouse.down();
    await page.mouse.move(box.x + 55, box.y + 15);
    await header.dispatchEvent("pointercancel", { pointerId: 1, clientX: box.x + 55, clientY: box.y + 15 });
    await page.mouse.up();
    expect(calls.filter((call) => call.method === "PUT")).toHaveLength(writesAfterSave);

    await page.mouse.move(box.x + 20, box.y + 15);
    await page.mouse.down();
    for (const offset of [35, 70, 105, 140]) await page.mouse.move(box.x + 20 + offset, box.y + 15);
    expect(calls.filter((call) => call.method === "PUT")).toHaveLength(writesAfterSave);
    await page.mouse.up();
    await expect.poll(() => calls.filter((call) => call.method === "PUT").length).toBe(writesAfterSave + 1);
    expect(calls.filter((call) => call.path.includes("/overview")).length - overviewAtLoad).toBe(0);

    const { violations } = await new AxeBuilder({ page }).analyze();
    expect(violations.filter((violation) => violation.impact === "serious" || violation.impact === "critical")).toEqual([]);
  });

  test("409 preserves local edits until explicit reload", async ({ page }) => {
    await reset(page);
    await page.getByRole("button", { name: "New draft" }).click();
    await page.getByRole("button", { name: "Add Events" }).click();
    await page.evaluate(async () => {
      const response = await fetch("/api/v1/dashboard-test/conflict-next", { method: "POST" });
      if (!response.ok) throw new Error(`conflict fixture failed: ${response.status}`);
    });
    await page.getByRole("button", { name: "Save changes" }).click();
    await expect(page.getByRole("alert")).toContainText("local edits are preserved");
    await expect(page.getByText(/unsaved/).first()).toBeVisible();
    await expect(page.getByRole("toolbar", { name: "Edit Events" })).toBeVisible();
    await page.getByRole("button", { name: "Reload latest" }).click();
    await expect(page.getByRole("alert")).toHaveCount(0);
    await expect(page.getByRole("toolbar", { name: "Edit Events" })).toHaveCount(0);
  });

  test("editor submits, publisher approves and exports, viewer fails closed", async ({ page }) => {
    const calls = trackBuilder(page);
    await reset(page);
    await page.getByRole("button", { name: "New draft" }).click();
    await expect.poll(() => calls.filter((call) => call.path.includes("/overview")).length).toBe(1);
    const editorOverviewStart = calls.filter((call) => call.path.includes("/overview")).length;
    await expect(page.getByRole("button", { name: "Submit for approval" })).toBeEnabled();
    await page.getByRole("button", { name: "Submit for approval" }).click();
    await expect(page.getByText(/submitted.*revision 2/).first()).toBeVisible();
    await expect(page.getByRole("button", { name: "Submit for approval" })).toHaveCount(0);
    expect(calls.filter((call) => call.path.includes("/overview")).length).toBe(editorOverviewStart);

    await page.goto("/dashboard-builder/11111111-1111-4111-8111-111111111111?scenario=builder-publisher&refresh=0");
    await expect(page.getByRole("button", { name: "Approve" })).toBeVisible();
    await expect.poll(() => calls.filter((call) => call.path.includes("/overview")).length).toBe(2);
    const publisherOverview = calls.filter((call) => call.path.includes("/overview")).length;
    await page.getByRole("button", { name: "Approve" }).click();
    await expect(page.getByText(/approved.*revision 3/).first()).toBeVisible();
    const downloadPromise = page.waitForEvent("download");
    await page.getByRole("button", { name: "Export canonical JSON" }).click();
    const download = await downloadPromise;
    expect(download.suggestedFilename()).toMatch(/^custom-[a-z0-9-]+\.json$/);
    const stream = await download.createReadStream();
    const chunks: Buffer[] = [];
    for await (const chunk of stream) chunks.push(Buffer.from(chunk));
    const exported = Buffer.concat(chunks);
    expect(exported.at(-1)).toBe(10);
    expect(exported.toString()).not.toContain("owner");
    expect(exported.toString()).not.toContain("revision");
    expect(createHash("sha256").update(exported).digest("hex")).toMatch(/^[a-f0-9]{64}$/);
    expect(calls.filter((call) => call.path.includes("/overview")).length).toBe(publisherOverview);

    await page.goto("/dashboard-builder?scenario=builder-viewer");
    await expect(page.getByText("You do not have dashboard permissions.")).toBeVisible();
    await expect(page.getByRole("button", { name: "New draft" })).toHaveCount(0);
    const status = await page.evaluate(async () => (await fetch("/api/v1/dashboard-drafts")).status);
    expect(status).toBe(403);
    const exportStatus = await page.evaluate(async () => (await fetch("/api/v1/dashboard-drafts/11111111-1111-4111-8111-111111111111/export")).status);
    expect(exportStatus).toBe(403);
  });
});
