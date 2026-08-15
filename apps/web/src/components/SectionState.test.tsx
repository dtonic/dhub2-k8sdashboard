import { render, screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { DegradedState } from "./SectionState";

describe("DegradedState", () => {
  it("describes an unconfigured alert history without claiming Alertmanager is unavailable", () => {
    render(<DegradedState source="Alertmanager" detail="history_not_configured" />);
    expect(screen.getByText("해소 이력 미구성")).toBeVisible();
    expect(screen.getByText("해소 이력 저장소가 구성되지 않아 현재 진행 중인 알림만 표시합니다.")).toBeVisible();
    expect(screen.queryByText("Alertmanager 응답 없음")).not.toBeInTheDocument();
  });
});
