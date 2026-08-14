import { defineConfig, devices } from "@playwright/test";
import path from "node:path";
import { fileURLToPath } from "node:url";

/**
 * 통합 E2E 설정 (#22).
 * --------------------------------------------------------------------------
 * mock(MSW) 스위트(playwright.config.ts)와 달리, 여기서는 **프로덕션 번들
 * (default mock-off dist) + 실제 Go BFF**를 검증합니다. 백엔드는 테스트 전용
 * e2efixture(가짜 informer + 시나리오 corpus + mock OIDC)이고, 클러스터·Docker가
 * 필요 없습니다. 실행 전 `make build-web-production`이 선행되어야 합니다
 * (`make test-web-integration`이 순서를 보장합니다).
 *
 * 데이터소스 중단은 픽스처 인스턴스 단위로 독립 선택합니다 — 포트마다 다른
 * 중단 상태의 서버가 떠서, 부분 장애 UX를 서로 오염 없이 검증합니다.
 */

const here = path.dirname(fileURLToPath(import.meta.url));
const apiDir = path.resolve(here, "../api");

/** 픽스처 오리진 — 스펙 파일이 같은 상수를 임포트합니다. */
export const FIXTURE = {
  main: "http://127.0.0.1:4273",
  greptimeOutage: "http://127.0.0.1:4274",
  quickwitOutage: "http://127.0.0.1:4275",
  alertsOutage: "http://127.0.0.1:4276",
} as const;

function fixtureServer(origin: string, outages?: string) {
  const port = new URL(origin).port;
	if (!/^\d{4,5}$/.test(port)) throw new Error(`unsafe fixture port: ${port}`);
	if (outages && !["greptime", "quickwit", "alerts"].includes(outages)) {
		throw new Error(`unsafe fixture outage: ${outages}`);
	}
  return {
    command:
      `go run -tags e2efixture ./cmd/e2efixture -addr 127.0.0.1:${port} -dist ../web/dist` +
      (outages ? ` -outages ${outages}` : ""),
    /* readyz는 informer 동기화가 끝난 뒤에만 리스너가 열리므로 200 = 준비 완료입니다. */
    url: `${origin}/readyz`,
    cwd: apiDir,
	/* 고정 포트는 stale fixture를 절대 재사용하지 않습니다. 충돌은 즉시 실패해야 합니다. */
    reuseExistingServer: false,
    timeout: 180_000,
    stdout: "pipe" as const,
    /* 이 호스트/CI 모두에서 toolchain auto 재실행 변수를 제거합니다. */
    env: { GOTOOLCHAIN: "local" },
  };
}

export default defineConfig({
  testDir: "./e2e-integration",
  fullyParallel: true,
  forbidOnly: !!process.env.CI,
  retries: process.env.CI ? 1 : 0,
  workers: process.env.CI ? 2 : undefined,
  reporter: [["list"]],
  timeout: 60_000,
  expect: { timeout: 10_000 },
  use: {
    baseURL: FIXTURE.main,
    /* 컨텍스트 헤더에 Bearer 토큰이 실립니다. trace/video 아티팩트로 남기지 않습니다. */
    trace: "off",
    video: "off",
    colorScheme: "dark",
    viewport: { width: 1600, height: 1000 },
  },
  projects: [{ name: "integration", use: { ...devices["Desktop Chrome"] } }],
  webServer: [
    fixtureServer(FIXTURE.main),
    fixtureServer(FIXTURE.greptimeOutage, "greptime"),
    fixtureServer(FIXTURE.quickwitOutage, "quickwit"),
    fixtureServer(FIXTURE.alertsOutage, "alerts"),
  ],
});
