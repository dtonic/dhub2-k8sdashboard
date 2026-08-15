import { defineConfig, devices } from "@playwright/test";

const baseURL = process.env.AUTH_FIXTURE_URL;
if (!baseURL?.startsWith("https://127.0.0.1:")) throw new Error("AUTH_FIXTURE_URL must be an owned loopback TLS origin");

export default defineConfig({
  testDir: "./e2e-auth",
  fullyParallel: false,
  workers: 1,
  reporter: [["list"]],
  timeout: 60_000,
  expect: { timeout: 10_000 },
  use: { baseURL, ignoreHTTPSErrors: true, trace: "off", video: "off" },
  projects: [{ name: "auth-session", use: { ...devices["Desktop Chrome"] } }],
});
