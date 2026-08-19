import { useEffect, useRef } from "react";

/**
 * 공통 확인 다이얼로그. window.confirm 대신 앱 내 모달을 씁니다 —
 * 위험 동작(배포·수정) 전 재확인 UI를 일관되게 제공하기 위함입니다.
 * Escape로 취소, 포커스는 확인 버튼에 둡니다.
 */
export function ConfirmDialog({
  open,
  title,
  message,
  confirmLabel = "확인",
  cancelLabel = "취소",
  tone = "warning",
  busy,
  onConfirm,
  onCancel,
}: {
  open: boolean;
  title: string;
  message: string;
  confirmLabel?: string;
  cancelLabel?: string;
  tone?: "warning" | "critical";
  busy?: boolean;
  onConfirm: () => void;
  onCancel: () => void;
}) {
  const confirmRef = useRef<HTMLButtonElement>(null);
  useEffect(() => {
    if (!open) return;
    confirmRef.current?.focus();
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") onCancel();
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [open, onCancel]);

  if (!open) return null;
  return (
    <div className="confirm-overlay" role="presentation" onClick={onCancel}>
      <div
        className="confirm-dialog"
        role="alertdialog"
        aria-modal="true"
        aria-labelledby="confirm-title"
        aria-describedby="confirm-message"
        onClick={(e) => e.stopPropagation()}
      >
        <h2 id="confirm-title" className="confirm-dialog__title">
          {title}
        </h2>
        <p id="confirm-message" className="confirm-dialog__message">
          {message}
        </p>
        <div className="confirm-dialog__actions">
          <button type="button" className="linkish" onClick={onCancel} disabled={busy}>
            {cancelLabel}
          </button>
          <button
            ref={confirmRef}
            type="button"
            className={tone === "critical" ? "btn btn--critical" : "btn btn--warning"}
            onClick={onConfirm}
            disabled={busy}
          >
            {busy ? "진행 중…" : confirmLabel}
          </button>
        </div>
      </div>
    </div>
  );
}
