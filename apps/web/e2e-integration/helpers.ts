import { request, type Browser, type BrowserContext, type Locator, type Page } from "@playwright/test";

/**
 * 통합 E2E 도우미 (#22).
 *
 * - 토큰은 픽스처의 POST /e2e/token(JSON 본문)으로 발급받아 **컨텍스트 헤더로만**
 *   전달합니다. 쿼리스트링·URL·콘솔·아티팩트에 토큰을 싣지 않습니다.
 * - 대기는 임의 sleep이 아니라 요소 가시성·응답 이벤트에 겁니다.
 */

export async function issueToken(baseURL: string, sub: string, roles: string[]): Promise<string> {
  const api = await request.newContext();
  try {
    const res = await api.post(`${baseURL}/e2e/token`, { data: { sub, roles } });
    if (!res.ok()) throw new Error(`토큰 발급 실패: ${res.status()}`);
    const body = (await res.json()) as { access_token: string };
    return body.access_token;
  } finally {
    await api.dispose();
  }
}

export interface AuthedSession {
  context: BrowserContext;
  page: Page;
}

/** 역할이 담긴 Bearer 컨텍스트를 만듭니다. 브라우저의 모든 요청에 헤더가 실립니다. */
export async function authedPage(browser: Browser, baseURL: string, sub: string, roles: string[]): Promise<AuthedSession> {
  const token = await issueToken(baseURL, sub, roles);
  const context = await browser.newContext({
    baseURL,
    extraHTTPHeaders: { authorization: `Bearer ${token}` },
  });
  const page = await context.newPage();
  return { context, page };
}

/** 화면이 실제로 몇 번 API를 호출하는지 셉니다. N+1 회귀 가드입니다. */
export function trackApi(page: Page) {
  const calls: string[] = [];
  page.on("request", (r) => {
    const u = new URL(r.url());
    if (u.pathname.startsWith("/api/")) calls.push(u.pathname + u.search);
  });
  return {
    all: () => calls,
    count: () => calls.length,
    matching: (re: RegExp) => calls.filter((c) => re.test(c)),
  };
}

/** 4xx/5xx 응답을 기록합니다. 화면 이동에서 나오면 deep link가 깨진 것입니다. */
export function trackFailures(page: Page) {
  const failures: string[] = [];
  page.on("response", (r) => {
    const u = new URL(r.url());
    if (u.pathname.startsWith("/api/") && r.status() >= 400) failures.push(`${r.status()} ${u.pathname}`);
  });
  return failures;
}

/**
 * "의미 있는 클릭"(운영자가 마우스로 하는 화면 전환 행동)을 세는 카운터.
 * 릴리스 기준: Overview에서 원인 Pod와 관련 로그까지 4클릭 이내. (#22)
 */
export function clickBudget() {
  let clicks = 0;
  return {
    click: async (locator: Locator) => {
      clicks += 1;
      await locator.click();
    },
    total: () => clicks,
  };
}
