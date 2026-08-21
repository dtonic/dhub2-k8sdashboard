import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";
import { UsageBar } from "./drill";

afterEach(cleanup);

describe("UsageBar", () => {
  it("기본(max=2)은 초과 할당 표현을 위해 100%를 막대 절반에 매핑하고 중앙 눈금을 그린다", () => {
    const { container } = render(<UsageBar ratio={0.93} label="CPU Request" />);
    const fill = container.querySelector<HTMLElement>(".usage__fill")!;
    expect(fill.style.width).toBe("46.5%");
    expect(container.querySelector(".usage__mark")).not.toBeNull();
  });

  it("max=1(노드 request/allocatable)은 0–100% 풀스케일이고 눈금이 없다 (#14)", () => {
    const { container } = render(<UsageBar ratio={0.93} label="Request" max={1} />);
    const fill = container.querySelector<HTMLElement>(".usage__fill")!;
    expect(fill.style.width).toBe("93%");
    expect(container.querySelector(".usage__mark")).toBeNull();
    expect(screen.getByText("93%")).toBeVisible();
  });

  it("max=1에서도 비율이 1을 넘으면 막대는 100%에서 멈추고 값은 그대로 표시한다", () => {
    const { container } = render(<UsageBar ratio={1.2} label="Request" max={1} />);
    expect(container.querySelector<HTMLElement>(".usage__fill")!.style.width).toBe("100%");
    expect(screen.getByText("120%")).toBeVisible();
  });
});
