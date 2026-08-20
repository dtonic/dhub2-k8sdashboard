import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { Combobox, type ComboboxOption } from "./Combobox";

/* 이 프로젝트 vitest 설정은 globals가 없어 RTL 자동 cleanup이 걸리지 않습니다 —
   id 중복 렌더가 누적되면 라벨 연결이 깨지므로 명시적으로 정리합니다. */
afterEach(cleanup);

/* 옵션이 수백 개여도 자르지 않고 검색·스크롤로 찾는다는 규칙을 검증합니다. (#1) */
const MANY: ComboboxOption[] = [
  { value: "all", label: "모든 Namespace" },
  ...Array.from({ length: 200 }, (_, i) => ({
    value: `ns-${String(i).padStart(3, "0")}`,
    label: `ns-${String(i).padStart(3, "0")}`,
  })),
];

function setup(options: ComboboxOption[] = MANY, value = "all") {
  const onSelect = vi.fn();
  render(<Combobox id="combo" label="네임스페이스" value={value} options={options} onSelect={onSelect} />);
  return { onSelect, input: screen.getByRole("combobox", { name: "네임스페이스" }) };
}

describe("Combobox", () => {
  it("열면 옵션을 자르지 않고 전부 렌더링한다", () => {
    const { input } = setup();
    fireEvent.click(input);
    expect(screen.getAllByRole("option")).toHaveLength(MANY.length);
  });

  it("타이핑하면 라벨 부분 일치로 필터되고, 일치가 없으면 안내를 보여준다", () => {
    const { input } = setup();
    fireEvent.click(input);
    fireEvent.change(input, { target: { value: "ns-19" } });
    /* ns-190 ~ ns-199 */
    expect(screen.getAllByRole("option")).toHaveLength(10);

    fireEvent.change(input, { target: { value: "grafana" } });
    expect(screen.queryAllByRole("option")).toHaveLength(0);
    expect(screen.getByText("일치하는 항목이 없습니다")).toBeVisible();
  });

  it("화살표로 이동하고 Enter로 선택한다", () => {
    const { input, onSelect } = setup();
    fireEvent.click(input);
    fireEvent.change(input, { target: { value: "ns-000" } });
    fireEvent.keyDown(input, { key: "ArrowDown" });
    fireEvent.keyDown(input, { key: "Enter" });
    expect(onSelect).toHaveBeenCalledWith("ns-000");
  });

  it("옵션 클릭으로 선택하고 목록이 닫힌다", () => {
    const { input, onSelect } = setup();
    fireEvent.click(input);
    fireEvent.click(screen.getByRole("option", { name: /ns-004/ }));
    expect(onSelect).toHaveBeenCalledWith("ns-004");
    expect(screen.queryByRole("listbox")).toBeNull();
  });

  it("disabled 옵션은 Enter·클릭 모두 선택되지 않는다", () => {
    const { input, onSelect } = setup([
      { value: "ok", label: "prod-seoul" },
      { value: "no", label: "prod-frankfurt", disabled: true, note: "권한 없음" },
    ]);
    fireEvent.click(input);
    fireEvent.click(screen.getByRole("option", { name: /prod-frankfurt/ }));
    fireEvent.change(input, { target: { value: "frankfurt" } });
    fireEvent.keyDown(input, { key: "Enter" });
    expect(onSelect).not.toHaveBeenCalled();
  });

  it("Esc는 검색어를 버리고 표시값을 현재 선택으로 되돌린다", () => {
    const { input } = setup();
    fireEvent.click(input);
    fireEvent.change(input, { target: { value: "ns-1" } });
    fireEvent.keyDown(input, { key: "Escape" });
    expect(screen.queryByRole("listbox")).toBeNull();
    expect((input as HTMLInputElement).value).toBe("모든 Namespace");
  });

  it("선택 상태는 색이 아니라 ✓ 표식과 aria-selected로 전달한다", () => {
    const { input } = setup(MANY, "ns-002");
    fireEvent.click(input);
    const selected = screen.getByRole("option", { name: /ns-002/ });
    expect(selected).toHaveAttribute("aria-selected", "true");
    expect(selected.textContent).toContain("✓");
  });
});
