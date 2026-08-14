import { render, screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { StatusBadge } from "./primitives";

describe("StatusBadge", () => {
  it("communicates status with text in addition to color", () => {
    render(<StatusBadge severity="critical" count={2} />);
    expect(screen.getByText("Critical")).toBeVisible();
    expect(screen.getByText("2")).toBeVisible();
    expect(screen.getByText("✕")).toHaveAttribute("aria-hidden", "true");
  });
});
