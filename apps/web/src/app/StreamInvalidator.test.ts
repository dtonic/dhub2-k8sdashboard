import { afterEach, describe, expect, it, vi } from "vitest";
import { createInvalidationCoalescer } from "./StreamInvalidator";

describe("createInvalidationCoalescer", () => {
  afterEach(() => vi.useRealTimers());
  it("coalesces a burst and cancels pending cleanup", () => {
    vi.useFakeTimers(); const invalidate=vi.fn(); const value=createInvalidationCoalescer(invalidate,250);
    for(let i=0;i<100;i++) value.schedule(); vi.advanceTimersByTime(249); expect(invalidate).not.toHaveBeenCalled(); vi.advanceTimersByTime(1); expect(invalidate).toHaveBeenCalledTimes(1);
    value.schedule(); value.cancel(); vi.runAllTimers(); expect(invalidate).toHaveBeenCalledTimes(1); expect(vi.getTimerCount()).toBe(0);
  });

  it("신호가 끊이지 않아도 flush는 minInterval에 한 번으로 묶인다 (#17)", () => {
    vi.useFakeTimers();
    const invalidate = vi.fn();
    const value = createInvalidationCoalescer(invalidate, 1_000, 15_000);
    /* 100ms마다 신호가 계속 들어오는 churn 클러스터를 흉내낸다. */
    value.schedule();
    for (let t = 0; t < 35_000; t += 100) {
      vi.advanceTimersByTime(100);
      value.schedule();
    }
    /* 기대 flush: 1s(첫 신호) → 16s → 31s. 250ms 폭풍처럼 수십 번이 아니어야 한다. */
    expect(invalidate).toHaveBeenCalledTimes(3);
    value.cancel();
    expect(vi.getTimerCount()).toBe(0);
  });
});
