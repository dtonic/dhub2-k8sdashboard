import { defineConfig, devices } from "@playwright/test";

const main = process.env.ALERTMANAGER_BROWSER_URL;
const outage = process.env.ALERTMANAGER_BROWSER_OUTAGE_URL;
if (!process.env.ALERTMANAGER_BROWSER_TOKEN) throw new Error("Alertmanager browser token sentinel is required");
for (const [name, value] of [["main", main], ["outage", outage]] as const) {
  if (!value?.startsWith("https://127.0.0.1:")) throw new Error(`${name} Alertmanager browser origin must be owned loopback TLS`);
}

export default defineConfig({
  testDir: "./e2e-alertmanager",
  fullyParallel: false,
  workers: 1,
  reporter: [["list"]],
  timeout: 60_000,
  expect: { timeout: 10_000 },
  use: { baseURL: main, ignoreHTTPSErrors: true, trace: "off", video: "off", colorScheme: "dark", viewport: { width: 1600, height: 1000 } },
  projects: [{ name: "alertmanager-live", use: { ...devices["Desktop Chrome"] } }],
});
