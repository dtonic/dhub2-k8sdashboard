import { describe, expect, it } from "vitest";
import type { ApiError, ResourceDescriptor } from "@k8s-dashboard/contracts";
import { HttpError } from "@/api/client";
import { explorerState, requestErrorMessage, stateFromError, stateNotice } from "./state";

const descriptor = (state: ResourceDescriptor["state"]): ResourceDescriptor => ({
  group: "core",
  version: "v1",
  resource: "services",
  kind: "Service",
  namespaced: true,
  verbs: ["get", "list", "watch"],
  state,
  count: 0,
});

/* 계약의 코드 union을 그대로 씁니다 — 오타나 폐기된 코드는 tsc가 잡아야 합니다. */
const err = (status: number, code: ApiError["code"]) => new HttpError(status, { code, message: "", requestId: "r" });

describe("Resource Explorer 상태 매핑", () => {
  it("서버 오류 코드를 상태로 옮긴다", () => {
    expect(stateFromError(err(503, "resources_unavailable"))).toBe("unavailable");
    expect(stateFromError(err(503, "resource_syncing"))).toBe("syncing");
    expect(stateFromError(err(502, "resource_unsupported"))).toBe("unsupported");
    expect(stateFromError(err(502, "resource_forbidden"))).toBe("forbidden");
    expect(stateFromError(err(404, "resource_not_served"))).toBe("missing");
    expect(stateFromError(err(404, "resource_not_allowlisted"))).toBe("missing");
  });

  it("상태가 아닌 오류를 상태로 위장하지 않는다", () => {
    expect(stateFromError(err(400, "invalid_filter"))).toBeUndefined();
    expect(stateFromError(err(409, "uid_mismatch"))).toBeUndefined();
    expect(stateFromError(new Error("network"))).toBeUndefined();
  });

  it("일곱 상태가 서로 다른 문구를 갖는다 — 같은 빈 화면으로 접히지 않는다", () => {
    const states = ["ready", "empty", "syncing", "unsupported", "forbidden", "missing", "unavailable"] as const;
    const titles = states.map((s) => stateNotice(s).title);
    expect(new Set(titles).size).toBe(states.length);
    for (const title of titles) expect(title.length).toBeGreaterThan(0);
  });

  it("0건과 동기화 중을 구분한다", () => {
    expect(explorerState(descriptor("ready"), undefined, 0, true)).toBe("empty");
    expect(explorerState(descriptor("syncing"), undefined, 0, true)).toBe("syncing");
    expect(explorerState(descriptor("ready"), undefined, 0, false)).toBe("ready");
    expect(explorerState(descriptor("ready"), undefined, 3, true)).toBe("ready");
  });

  it("목록 오류 상태가 카탈로그 상태보다 우선한다", () => {
    expect(explorerState(descriptor("ready"), err(502, "resource_unsupported"), 0, true)).toBe("unsupported");
  });

  it("상태가 아닌 오류는 사용자에게 이유를 그대로 알린다", () => {
    expect(requestErrorMessage(err(400, "invalid_filter"))).toContain("상한");
    expect(requestErrorMessage(err(409, "uid_mismatch"))).toContain("교체");
    expect(requestErrorMessage(err(429, "detail_rate_limited"))).toContain("한도");
    expect(requestErrorMessage(err(404, "not_found"))).toContain("목록에 없는");
    /* 상태로 처리되는 코드는 오류 문구를 만들지 않습니다 — 두 번 알리지 않습니다. */
    expect(requestErrorMessage(err(503, "resource_syncing"))).toBeUndefined();
  });
});
