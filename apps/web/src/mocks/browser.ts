import { setupWorker } from "msw/browser";
import { handlers } from "./handlers";

export const worker = setupWorker(...handlers);

/**
 * 개발 서버와 명시적 `--mode e2e` 번들의 Mock API를 시작합니다.
 * Production은 기본 mock-off이고 명시적인 `VITE_USE_MOCK=true`에서만 호출합니다.
 */
export async function startMockApi() {
  await worker.start({
    onUnhandledRequest: "bypass",
    quiet: true,
    serviceWorker: { url: "/mockServiceWorker.js" },
  });
}
