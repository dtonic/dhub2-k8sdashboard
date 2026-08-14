import { defineConfig, devices } from "@playwright/test";

/**
 * E2E 설정.
 * --------------------------------------------------------------------------
 * 빌드된 산출물을 `vite preview`로 띄워 테스트합니다. dev 서버가 아니라 빌드 결과를
 * 검증해야 프로덕션 번들에서만 나는 문제(사이드이펙트 트리셰이킹, MSW 워커 경로 등)를 잡습니다.
 *
 * 기존 53개 회귀 테스트는 `--mode e2e`로 MSW를 명시적으로 켭니다.
 * 실제 Go BFF fixture 검증은 `make test-web-integration`의 별도 스위트가 담당합니다.
 */
export default defineConfig({
  testDir: "./e2e",
  fullyParallel: true,
  forbidOnly: !!process.env.CI,
  retries: process.env.CI ? 1 : 0,
  workers: process.env.CI ? 2 : undefined,
  reporter: process.env.CI ? [["list"], ["html", { open: "never" }]] : [["list"]],
  timeout: 45_000,
  expect: { timeout: 10_000 },
  use: {
    baseURL: process.env.E2E_BASE_URL ?? "http://127.0.0.1:4173",
    trace: "on-first-retry",
    /* 관제 화면은 Dark 사용 비중이 높습니다. 기본을 Dark로 두고 Light는 별도 프로젝트로 돕니다. */
    colorScheme: "dark",
    viewport: { width: 1600, height: 1000 },
  },
  projects: [
    { name: "dark", use: { ...devices["Desktop Chrome"], colorScheme: "dark" } },
    { name: "light", use: { ...devices["Desktop Chrome"], colorScheme: "light" }, testMatch: /smoke\.spec\.ts/ },
  ],
  webServer: process.env.E2E_BASE_URL
    ? undefined
    : {
        /* readiness가 127.0.0.1을 폴링하므로 바인딩도 127.0.0.1로 고정합니다.
         * 기본 host(localhost)는 환경에 따라 ::1(IPv6)에만 바인딩되어 180초 타임아웃이 났습니다. */
        command: "npm run build -- --mode e2e && npx vite preview --host 127.0.0.1 --port 4173 --strictPort",
        url: "http://127.0.0.1:4173",
        reuseExistingServer: !process.env.CI,
        timeout: 180_000,
        /* CI 로그에 서버 기동 출력을 남겨 다음 진단을 쉽게 합니다. */
        stdout: "pipe",
      },
});
