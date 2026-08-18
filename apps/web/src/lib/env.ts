/**
 * Mock API 사용 여부 — main.tsx의 부트스트랩과 AppShell 배지가 **같은 식**을 봐야
 * "Mock API" 표시가 실제 동작 모드와 어긋나지 않습니다. (#27)
 * 프로덕션은 mock-off가 기본이고 명시적 `VITE_USE_MOCK=true`에서만 켭니다.
 */
export const usingMockApi =
  import.meta.env.VITE_USE_MOCK === "true" || (import.meta.env.DEV && import.meta.env.VITE_USE_MOCK !== "false");
