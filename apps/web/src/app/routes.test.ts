import { readFileSync } from "node:fs";
import { join } from "node:path";
import { describe, expect, it } from "vitest";

/**
 * 라우트 보존 가드 (ADR 0018 · ADR 0023)
 * --------------------------------------------------------------------------
 * Resources 진입점은 **추가**입니다. 기존 관리 라우트를 리다이렉트하거나 없애면
 * 기존 링크·북마크·E2E가 조용히 깨지므로, 라우트 표를 소스에서 직접 읽어 확인합니다.
 * (apps/api의 TestOpenAPIMatchesRouter와 같은 방식입니다.)
 *
 * ADR 0023에서 경로 집합의 단일 원천이 `src/app/routes.tsx`로 옮겨졌습니다. 그래서
 * 이 가드는 **두 파일을 함께** 읽습니다.
 *
 *   - `routes.tsx` — 경로와 element의 정의. 무엇이 어떤 화면을 가리키는지.
 *   - `main.tsx`   — 그 배열을 그대로 `<Route>`로 펼치는지, catch-all이 하나뿐인지.
 *
 * 한쪽만 보면 "레지스트리에는 있는데 라우터가 안 읽는" 상태나 그 반대를 놓칩니다.
 * 런타임 매핑은 `routes.test.tsx`가 실제 렌더로 다시 확인합니다 — 이 파일은 소스에
 * 남아야 할 사실을, 저 파일은 화면에 나와야 할 결과를 지킵니다.
 */
/* vitest의 import.meta.url은 file: 스킴이 아닐 수 있으므로 cwd(apps/web) 기준으로 읽습니다. */
const registry = readFileSync(join(process.cwd(), "src", "app", "routes.tsx"), "utf8");
const main = readFileSync(join(process.cwd(), "src", "main.tsx"), "utf8");

/** 레지스트리 한 항목에서 `path`에 대응하는 element 표기를 그대로 꺼냅니다. */
function routeElement(path: string): string | undefined {
  const match = new RegExp(`path:\\s*"${path}"[^\\n]*?element:\\s*(<[^\\n]*?/>)`).exec(registry);
  return match?.[1]?.trim();
}

describe("라우트 표", () => {
  it("기존 관리 라우트가 같은 화면을 그대로 가리킨다", () => {
    expect(routeElement("/deployments")).toBe('<ManageView kind="deployments" />');
    expect(routeElement("/secrets")).toBe('<ManageView kind="secrets" />');
  });

  it("관리·Resources 라우트를 리다이렉트로 바꾸지 않았다", () => {
    for (const path of ["/deployments", "/secrets", "/resources"]) {
      const element = routeElement(path) ?? "";
      expect(element, `${path} element를 찾지 못했습니다`).not.toBe("");
      expect(element).not.toContain("Navigate");
      expect(element).not.toContain("redirect");
    }
    /* 레지스트리에는 리다이렉트가 하나도 없습니다 — 전부 실제 화면입니다. */
    expect(registry).not.toContain("Navigate");
    /* catch-all(*) 하나만 Navigate를 씁니다 — 기존 동작 그대로입니다. */
    expect(main.match(/<Navigate/g) ?? []).toHaveLength(1);
    expect(main).toContain('<Route path="*" element={<Navigate to="/" replace />} />');
  });

  it("라우터가 레지스트리를 그대로 펼치고, 손으로 쓴 라우트가 남아 있지 않다", () => {
    expect(main).toContain('import { APP_ROUTES } from "@/app/routes"');
    /* 배열의 모든 항목이 path와 element를 그대로 가지고 <Route>가 됩니다. */
    expect(main).toMatch(/APP_ROUTES\.map\(\(route\) => \(\s*<Route key=\{route\.id\} path=\{route\.path\} element=\{route\.element\} \/>/);
    /* 리터럴 path는 catch-all 하나뿐입니다 — 레지스트리를 우회한 라우트가 없습니다. */
    expect(main.match(/path="/g) ?? []).toHaveLength(1);
    expect(main).toContain('path="*"');
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
      expect(registry, `${path} 라우트가 사라졌습니다`).toContain(`path: "${path}"`);
    }
  });
});
