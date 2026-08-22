import { useEffect, type RefObject } from "react";
import { isEditableTarget, modalOpen } from "@/app/keyboard";

/**
 * `/`로 이름 검색 입력에 포커스 (ADR 0023 Phase 3)
 * --------------------------------------------------------------------------
 * Resources 화면에서만 붙습니다. 팔레트를 열지 않고 **이미 화면에 있는 입력**으로
 * 포커스를 옮기기만 합니다 — 화면 안에서 찾는 일과 전역으로 찾는 일은 다른 동작이고,
 * `/` 하나로 둘을 섞으면 어느 쪽도 예측되지 않습니다.
 *
 * 다음 경우에는 **동작하지 않습니다.** 하나라도 빠지면 타이핑이 사라집니다.
 *
 *   - 포커스가 input·textarea·select·contenteditable 안에 있을 때
 *     (검색어에 `/`를 넣는 것이 정상 입력입니다)
 *   - IME 조합 중일 때 (조합 문자를 끊습니다)
 *   - 모달·다이얼로그가 열려 있을 때 (그 뒤의 입력으로 포커스를 훔칩니다)
 *   - 수정자 키가 함께 눌렸을 때 (다른 단축키와 겹칩니다)
 */
export function useSlashFocus(target: RefObject<HTMLElement | null>): void {
  useEffect(() => {
    const onKeyDown = (event: KeyboardEvent) => {
      if (event.key !== "/") return;
      if (event.metaKey || event.ctrlKey || event.altKey) return;
      if (event.isComposing) return;
      if (isEditableTarget(event.target)) return;
      if (modalOpen()) return;
      const el = target.current;
      if (!el) return;
      event.preventDefault();
      el.focus();
      /* 이미 값이 있으면 덮어쓰기 쉽도록 전체 선택합니다. */
      if (el instanceof HTMLInputElement) el.select();
    };
    window.addEventListener("keydown", onKeyDown);
    return () => window.removeEventListener("keydown", onKeyDown);
  }, [target]);
}
