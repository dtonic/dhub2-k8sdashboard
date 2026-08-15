import { describe, expect, it } from "vitest";
import { alertSectionCount } from "./AlertsView";

describe("alertSectionCount", () => {
  it("does not render unavailable or unconfigured sections as zero alerts", () => {
    expect(alertSectionCount(undefined)).toBe("—");
    expect(alertSectionCount({ status: "degraded" })).toBe("—");
    expect(alertSectionCount({ status: "error", data: [] })).toBe("—");
  });

  it("renders counts only for successful sections", () => {
    expect(alertSectionCount({ status: "ok" })).toBe("0");
    expect(alertSectionCount({ status: "ok", data: [{}, {}] })).toBe("2");
  });
});
