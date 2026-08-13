import { setupWorker } from "msw/browser";
import { handlers } from "./handlers";

export const worker = setupWorker(...handlers);

/**
 * Mock API를 켭니다. 실제 API가 붙으면 `VITE_USE_MOCK=false`로 끄고
 * 같은 계약(@k8s-dashboard/contracts)을 그대로 사용합니다.
 */
export async function startMockApi() {
  await worker.start({
    onUnhandledRequest: "bypass",
    quiet: true,
    serviceWorker: { url: "/mockServiceWorker.js" },
  });
}
