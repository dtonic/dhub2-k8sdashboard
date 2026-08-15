import { describe, expect, it, vi } from "vitest";
import { parseAuthSession } from "./session";

describe("parseAuthSession", () => {
  const base = { authenticated: true as const, principal: { displayName: "operator" }, capabilities: { canEditDashboard: true, canPublishDashboard: false }, csrfToken: "c".repeat(43) };

  it("accepts a due refresh despite delayed parsing or a client clock ahead", () => {
    vi.setSystemTime(new Date("2030-01-01T00:01:00Z"));
    expect(parseAuthSession({ ...base, refreshAt: "2030-01-01T00:00:00.125Z", expiresAt: "2030-01-01T00:02:00Z" }).authenticated).toBe(true);
    vi.useRealTimers();
  });

  it.each([
    ["non-RFC3339", "2030-01-01 00:00:00Z", "2030-01-01T00:02:00Z"],
    ["wrong ordering", "2030-01-01T00:02:00Z", "2030-01-01T00:02:00Z"],
  ])("rejects %s timestamps", (_name, refreshAt, expiresAt) => {
    expect(() => parseAuthSession({ ...base, refreshAt, expiresAt })).toThrow("invalid session");
  });
});
