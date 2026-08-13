import type { Page, Request } from "@playwright/test";

/** 화면이 실제로 몇 번 API를 호출하는지 세는 도우미. N+1 회귀를 잡는 데 씁니다. */
export function trackApi(page: Page) {
  const calls: string[] = [];
  page.on("request", (r: Request) => {
    const u = new URL(r.url());
    if (u.pathname.startsWith("/api/")) calls.push(u.pathname + u.search);
  });
  return {
    all: () => calls,
    count: () => calls.length,
    since: (n: number) => calls.slice(n),
    matching: (re: RegExp) => calls.filter((c) => re.test(c)),
  };
}

/** 응답 상태를 감시합니다. 화면 이동에서 4xx/5xx가 나면 deep link가 깨진 것입니다. */
export function trackFailures(page: Page) {
  const failures: string[] = [];
  page.on("response", (r) => {
    const u = new URL(r.url());
    if (u.pathname.startsWith("/api/") && r.status() >= 400) failures.push(`${r.status()} ${u.pathname}`);
  });
  return failures;
}

/** 목록이 로드될 때까지 기다립니다. 로딩 골격과 실제 값을 구분합니다. */
export async function waitForData(page: Page) {
  await page.waitForLoadState("networkidle");
  await page.waitForTimeout(250);
}
