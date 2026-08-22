/**
 * 전역 단축키가 공유하는 판정 (ADR 0023)
 * --------------------------------------------------------------------------
 * 팔레트(`CommandPalette`)와 `/` 포커스(`useSlashFocus`)가 같은 규칙을 봅니다.
 * 두 곳에 복붙하면 한쪽만 고쳐져 "어떤 화면에서는 타이핑이 사라지는" 차이가 생깁니다.
 *
 * 이 모듈은 **컴포넌트를 가져오지 않습니다.** 훅이 컴포넌트를 import하면
 * `routes → ResourcesView → ResourceExplorer → useSlashFocus → CommandPalette → routes`로
 * 순환이 생겨 모듈 초기화 순서에 따라 undefined가 됩니다.
 */

/**
 * 열려 있는 모달의 선택자.
 *
 * `alertdialog`도 함께 봅니다 — `ConfirmDialog`가 쓰는 role이고, 파괴적 동작을
 * 확인받는 창입니다. 이걸 빠뜨리면 확인 창 위에 팔레트가 겹치거나 `/`가 확인 창
 * 뒤의 입력으로 포커스를 훔쳐, 사용자가 무엇에 답하는지 모르는 채로 Enter를 칩니다.
 */
export const MODAL_SELECTOR = '[role="dialog"][aria-modal="true"], [role="alertdialog"][aria-modal="true"]';

/** 이미 열려 있는 모달이 있는지. 그 뒤의 요소로 포커스를 훔치지 않습니다. */
export function modalOpen(): boolean {
  return document.querySelector(MODAL_SELECTOR) !== null;
}

/** 편집 중인 요소인지. 여기서 true면 단축키가 타이핑을 가로채면 안 됩니다. */
export function isEditableTarget(target: EventTarget | null): boolean {
  const el = target as HTMLElement | null;
  if (!el || typeof el.tagName !== "string") return false;
  const tag = el.tagName.toLowerCase();
  if (tag === "input" || tag === "textarea" || tag === "select") return true;
  return el.isContentEditable === true;
}

/** Tab 순서에 들어가는 요소들. `tabindex="-1"`은 프로그램 포커스 전용이라 제외합니다. */
const FOCUSABLE_SELECTOR = [
  "a[href]",
  "button:not([disabled])",
  "input:not([disabled])",
  "textarea:not([disabled])",
  "select:not([disabled])",
  '[tabindex]:not([tabindex="-1"])',
].join(", ");

/**
 * 모달 안에서 Tab이 돌 수 있는 요소들.
 *
 * 가시성으로 거르지 않습니다 — jsdom에는 레이아웃이 없어 `offsetParent`가 항상
 * null이므로, 그걸로 거르면 테스트에서만 트랩이 비고 브라우저에서만 동작하는
 * 코드가 됩니다. 팔레트는 숨긴 포커스 대상을 두지 않습니다.
 */
export function focusablesIn(root: HTMLElement): HTMLElement[] {
  return Array.from(root.querySelectorAll<HTMLElement>(FOCUSABLE_SELECTOR));
}
