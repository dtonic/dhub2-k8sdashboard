import { useEffect, useId, useRef, useState, type KeyboardEvent } from "react";

/**
 * 검색형 단일 선택 콤보박스. (#1)
 * --------------------------------------------------------------------------
 * 네이티브 <select>는 옵션이 수백 개일 때 검색이 불가능해 쓸 수 없습니다.
 * ARIA 1.2 combobox 패턴(input[role=combobox] + [role=listbox])으로 대체합니다.
 *
 * - 목록은 max-height를 넘으면 스크롤합니다. 옵션 개수를 자르지 않습니다.
 * - 타이핑하면 라벨 부분 일치로 즉시 필터됩니다. 필터는 표시용일 뿐이고
 *   실제 권한 판정은 서버가 합니다 (README §10).
 * - Esc·바깥 클릭·blur는 선택을 바꾸지 않고 표시값을 현재 선택으로 되돌립니다.
 */
export type ComboboxOption = {
  value: string;
  label: string;
  disabled?: boolean;
  /** 옵션 오른쪽 보조 표기 (예: "권한 없음") */
  note?: string;
};

export function Combobox({
  id,
  label,
  value,
  options,
  onSelect,
  disabled,
}: {
  /** input 요소의 id — 기존 <select> 자리를 물려받아 테스트·라벨 연결이 유지됩니다. */
  id: string;
  /** 접근성 라벨 (시각적으로 숨김) */
  label: string;
  value: string;
  options: ComboboxOption[];
  onSelect: (value: string) => void;
  disabled?: boolean;
}) {
  const [open, setOpen] = useState(false);
  /** null이면 검색 중이 아님 — 표시값은 현재 선택의 라벨입니다. */
  const [query, setQuery] = useState<string | null>(null);
  const [active, setActive] = useState(0);
  const rootRef = useRef<HTMLDivElement>(null);
  const listRef = useRef<HTMLUListElement>(null);
  const listId = useId();

  const selected = options.find((o) => o.value === value);
  const display = query ?? selected?.label ?? "";
  const needle = (query ?? "").trim().toLowerCase();
  const shown = needle ? options.filter((o) => o.label.toLowerCase().includes(needle)) : options;

  const close = () => {
    setOpen(false);
    setQuery(null);
  };

  const openList = () => {
    setOpen(true);
    setActive(Math.max(0, options.findIndex((o) => o.value === value)));
  };

  const pick = (o: ComboboxOption | undefined) => {
    if (!o || o.disabled) return;
    onSelect(o.value);
    close();
  };

  /* 바깥 클릭으로 닫기 — blur만 쓰면 옵션 클릭(pointerdown)과 경합합니다. */
  useEffect(() => {
    if (!open) return;
    const onDown = (e: PointerEvent) => {
      if (rootRef.current && !rootRef.current.contains(e.target as Node)) close();
    };
    document.addEventListener("pointerdown", onDown);
    return () => document.removeEventListener("pointerdown", onDown);
  }, [open]);

  /* 활성 옵션을 스크롤로 따라갑니다 — 목록이 스크롤될 만큼 길기 때문입니다.
     (jsdom 등 scrollIntoView가 없는 환경에서는 조용히 건너뜁니다) */
  useEffect(() => {
    if (!open) return;
    const el = listRef.current?.querySelector<HTMLElement>(`[data-index="${active}"]`);
    el?.scrollIntoView?.({ block: "nearest" });
  }, [open, active]);

  const onKeyDown = (e: KeyboardEvent<HTMLInputElement>) => {
    if (e.key === "ArrowDown" || e.key === "ArrowUp") {
      e.preventDefault();
      if (!open) return openList();
      const dir = e.key === "ArrowDown" ? 1 : -1;
      setActive((a) => Math.min(shown.length - 1, Math.max(0, a + dir)));
      return;
    }
    if (e.key === "Enter") {
      if (!open) return;
      e.preventDefault();
      pick(shown[active]);
      return;
    }
    if (e.key === "Escape") {
      if (!open) return;
      e.preventDefault();
      close();
    }
  };

  return (
    <div className="ds-combobox" ref={rootRef}>
      <label className="visually-hidden" htmlFor={id}>
        {label}
      </label>
      <input
        id={id}
        className="ds-combobox__input"
        role="combobox"
        aria-expanded={open}
        aria-controls={listId}
        aria-autocomplete="list"
        aria-activedescendant={open && shown[active] ? `${id}-opt-${active}` : undefined}
        value={display}
        placeholder={selected?.label ?? label}
        disabled={disabled}
        autoComplete="off"
        spellCheck={false}
        onClick={() => {
          if (!open) openList();
        }}
        onChange={(e) => {
          if (!open) setOpen(true);
          setQuery(e.target.value);
          setActive(0);
        }}
        onKeyDown={onKeyDown}
        onBlur={(e) => {
          /* Tab 이동 등 컴포넌트 밖으로 나가면 닫습니다. 목록 클릭은 pointerdown에서
             preventDefault로 blur 자체를 막습니다. */
          if (!rootRef.current?.contains(e.relatedTarget as Node)) close();
        }}
      />
      <span className="ds-combobox__caret" aria-hidden="true">
        ▾
      </span>
      {open && (
        <ul className="ds-combobox__list" role="listbox" id={listId} ref={listRef} aria-label={label}>
          {shown.length === 0 && <li className="ds-combobox__empty">일치하는 항목이 없습니다</li>}
          {shown.map((o, i) => (
            <li
              key={o.value}
              id={`${id}-opt-${i}`}
              data-index={i}
              role="option"
              aria-selected={o.value === value}
              aria-disabled={o.disabled || undefined}
              className={[
                "ds-combobox__option",
                i === active ? "ds-combobox__option--active" : "",
                o.disabled ? "ds-combobox__option--disabled" : "",
              ]
                .filter(Boolean)
                .join(" ")}
              onPointerDown={(e) => e.preventDefault()}
              onPointerMove={() => setActive(i)}
              onClick={() => pick(o)}
            >
              <span className="ds-combobox__check" aria-hidden="true">
                {o.value === value ? "✓" : ""}
              </span>
              <span className="ds-combobox__label">{o.label}</span>
              {o.note && <span className="ds-combobox__note">{o.note}</span>}
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}
