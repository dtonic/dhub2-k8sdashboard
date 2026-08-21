import { readFileSync } from "node:fs";
import { join } from "node:path";
import { describe, expect, it } from "vitest";

/**
 * 라우트 보존 가드 (ADR 0018)
 * --------------------------------------------------------------------------
 * Resources 진입점은 **추가**입니다. 기존 관리 라우트를 리다이렉트하거나 없애면
 * 기존 링크·북마크·E2E가 조용히 깨지므로, 라우트 표를 소스에서 직접 읽어 확인합니다.
 * (apps/api의 TestOpenAPIMatchesRouter와 같은 방식입니다.)
 */
/* vitest의 import.meta.url은 file: 스킴이 아닐 수 있으므로 cwd(apps/web) 기준으로 읽습니다. */
const source = readFileSync(join(process.cwd(), "src", "main.tsx"), "utf8");

function routeElement(path: string): string | undefined {
  const match = new RegExp(`<Route\\s+path="${path.replace("/", "\\/")}"\\s+element=\\{([^}]*)\\}`).exec(source);
  return match?.[1]?.trim();
}

describe("라우트 표", () => {
  it("기존 관리 라우트가 같은 화면을 그대로 가리킨다", () => {
    expect(routeElement("/deployments")).toBe('<ManageView kind="deployments" />');
    expect(routeElement("/secrets")).toBe('<ManageView kind="secrets" />');
  });

  it("관리 라우트를 리다이렉트로 바꾸지 않았다", () => {
    for (const path of ["/deployments", "/secrets"]) {
      const element = routeElement(path) ?? "";
      expect(element).not.toContain("Navigate");
      expect(element).not.toContain("redirect");
    }
    /* catch-all(*) 하나만 Navigate를 씁니다 — 기존 동작 그대로입니다. */
    expect(source.match(/<Navigate/g) ?? []).toHaveLength(1);
  });

  it("Resources 라우트가 추가되어 있다", () => {
    expect(routeElement("/resources")).toBe("<ResourcesView />");
  });

  it("기존 화면 라우트가 하나도 사라지지 않았다", () => {
    for (const path of [
      "/",
      "/nodes",
      "/namespaces",
      "/namespaces/:namespace",
      "/workloads/:kind/:name",
      "/pods/:name",
      "/topology",
      "/logs",
      "/alerts",
      "/deployments",
      "/secrets",
      "/dashboards/:id",
      "/dashboard-builder",
      "/dashboard-builder/:id",
    ]) {
      expect(source, `${path} 라우트가 사라졌습니다`).toContain(`path="${path}"`);
    }
  });
});
