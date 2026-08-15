import { afterEach, describe, expect, it, vi } from "vitest";
import { createInvalidationCoalescer } from "./StreamInvalidator";

describe("createInvalidationCoalescer", () => {
  afterEach(() => vi.useRealTimers());
  it("coalesces a burst and cancels pending cleanup", () => {
    vi.useFakeTimers(); const invalidate=vi.fn(); const value=createInvalidationCoalescer(invalidate,250);
    for(let i=0;i<100;i++) value.schedule(); vi.advanceTimersByTime(249); expect(invalidate).not.toHaveBeenCalled(); vi.advanceTimersByTime(1); expect(invalidate).toHaveBeenCalledTimes(1);
    value.schedule(); value.cancel(); vi.runAllTimers(); expect(invalidate).toHaveBeenCalledTimes(1); expect(vi.getTimerCount()).toBe(0);
  });
});
