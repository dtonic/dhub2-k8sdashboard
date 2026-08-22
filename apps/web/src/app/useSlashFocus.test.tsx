import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { useRef } from "react";
import { afterEach, describe, expect, it } from "vitest";
import { useSlashFocus } from "./useSlashFocus";

afterEach(cleanup);

/**
 * jsdom은 `HTMLElement.isContentEditable`을 구현하지 않아 항상 `undefined`입니다.
 * 브라우저에서는 `contenteditable` 요소가 `true`를 돌려주므로 **fixture에서만** 그
 * 값을 정의해 실제 동작을 맞춥니다 — 판정 자체(`keyboard.ts`)는 그대로 두고,
 * 하네스가 브라우저와 다르게 굴어서 통과하는 일이 없게 합니다.
 */
function emulateContentEditable(el: HTMLDivElement | null): void {
  if (el && el.isContentEditable !== true) {
    Object.defineProperty(el, "isContentEditable", { value: true, configurable: true });
  }
}

function Harness({
  withDialog = false,
  dialogRole = "dialog",
}: {
  withDialog?: boolean;
  dialogRole?: "dialog" | "alertdialog";
}) {
  const ref = useRef<HTMLInputElement>(null);
  useSlashFocus(ref);
  return (
    <div>
      <input aria-label="이름 prefix" ref={ref} defaultValue="payments-" />
      <input aria-label="다른 입력" />
      <textarea aria-label="메모" />
      <div
        aria-label="편집 영역"
        contentEditable
        suppressContentEditableWarning
        tabIndex={0}
        ref={emulateContentEditable}
      />
      <button type="button">버튼</button>
      {withDialog && (
        <div role={dialogRole} aria-modal="true" aria-label="열린 모달">
          <input aria-label="모달 입력" />
        </div>
      )}
    </div>
  );
}

const target = () => screen.getByLabelText("이름 prefix");

describe("'/' 포커스", () => {
  it("본문에서 누르면 이름 입력으로 포커스가 갑니다", () => {
    render(<Harness />);
    const button = screen.getByRole("button", { name: "버튼" });
    button.focus();
    /* 실제 요소에서 dispatch해 window 리스너까지 버블링시킵니다 — `target` 옵션으로
       넘기면 Testing Library가 window에 target을 대입하려다 실패합니다. */
    fireEvent.keyDown(button, { key: "/" });
    expect(target()).toHaveFocus();
  });

  it("이미 값이 있으면 전체 선택해 덮어쓰기 쉽게 합니다", () => {
    render(<Harness />);
    fireEvent.keyDown(document.body, { key: "/" });
    const input = target() as HTMLInputElement;
    expect(input.selectionStart).toBe(0);
    expect(input.selectionEnd).toBe(input.value.length);
  });

  it("입력·textarea·contenteditable 안에서는 동작하지 않습니다", () => {
    render(<Harness />);
    for (const label of ["다른 입력", "메모", "편집 영역"]) {
      const el = screen.getByLabelText(label);
      el.focus();
      fireEvent.keyDown(el, { key: "/" });
      expect(target()).not.toHaveFocus();
    }
  });

  it("IME 조합 중에는 동작하지 않습니다", () => {
    render(<Harness />);
    fireEvent.keyDown(document.body, { key: "/", isComposing: true });
    expect(target()).not.toHaveFocus();
  });

  it("모달이 열려 있으면 뒤의 입력으로 포커스를 훔치지 않습니다", () => {
    render(<Harness withDialog />);
    fireEvent.keyDown(document.body, { key: "/" });
    expect(target()).not.toHaveFocus();
  });

  it("확인 창(alertdialog) 뒤로도 포커스를 훔치지 않습니다", () => {
    /* ConfirmDialog는 role="alertdialog"입니다. 이걸 빠뜨리면 파괴적 동작을
       확인받는 창 뒤의 입력으로 포커스가 가고, 사용자는 확인 창에 답한다고
       믿으면서 검색어를 칩니다. */
    render(<Harness withDialog dialogRole="alertdialog" />);
    fireEvent.keyDown(document.body, { key: "/" });
    expect(target()).not.toHaveFocus();
  });

  it("수정자 키가 함께 눌리면 무시합니다", () => {
    render(<Harness />);
    for (const mod of [{ ctrlKey: true }, { metaKey: true }, { altKey: true }]) {
      fireEvent.keyDown(document.body, { key: "/", ...mod });
      expect(target()).not.toHaveFocus();
    }
  });

  it("팔레트를 열지 않습니다 — 이 훅은 포커스만 옮깁니다", () => {
    render(<Harness />);
    fireEvent.keyDown(document.body, { key: "/" });
    expect(screen.queryByRole("dialog", { name: "명령 팔레트" })).not.toBeInTheDocument();
  });
});
