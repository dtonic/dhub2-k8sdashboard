import { useEffect, useRef } from "react";
import type { ResourceDetailResponse } from "@k8s-dashboard/contracts";
import { requestErrorMessage } from "./state";

/**
 * 선택한 항목의 **읽기 전용** 매니페스트 (ADR 0018 결정 5·6)
 * --------------------------------------------------------------------------
 * 서버가 정제해 내려준 YAML을 그대로 보여주기만 합니다. 편집·저장·복사 경로가
 * 없고, Secret의 data/stringData는 애초에 응답에 없습니다. 무엇이 제거됐는지는
 * 숨기지 않고 표시합니다 — 가려졌다는 사실 자체가 정보입니다.
 */
export function ResourceDetailDrawer({
  open,
  loading,
  detail,
  error,
  onClose,
}: {
  open: boolean;
  loading: boolean;
  detail: ResourceDetailResponse | undefined;
  error: unknown;
  onClose: () => void;
}) {
  const closeRef = useRef<HTMLButtonElement>(null);
  const dialogRef = useRef<HTMLDivElement>(null);

  useEffect(() => {
    if (!open) return;
    closeRef.current?.focus();
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") {
        onClose();
        return;
      }
      /* 열려 있는 동안 포커스는 대화상자 안에 머뭅니다. */
      if (e.key !== "Tab" || !dialogRef.current) return;
      const focusable = dialogRef.current.querySelectorAll<HTMLElement>(
        'button, [href], textarea, input, select, [tabindex]:not([tabindex="-1"])',
      );
      if (focusable.length === 0) return;
      const first = focusable[0];
      const last = focusable[focusable.length - 1];
      if (e.shiftKey && document.activeElement === first) {
        e.preventDefault();
        last.focus();
      } else if (!e.shiftKey && document.activeElement === last) {
        e.preventDefault();
        first.focus();
      }
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [open, onClose]);

  if (!open) return null;
  const message = requestErrorMessage(error);

  return (
    <div className="resource-drawer__overlay" role="presentation" onClick={onClose}>
      <div
        ref={dialogRef}
        className="resource-drawer"
        role="dialog"
        aria-modal="true"
        aria-labelledby="resource-drawer-title"
        onClick={(e) => e.stopPropagation()}
      >
        <div className="resource-drawer__header">
          <div>
            <h2 id="resource-drawer-title" className="resource-drawer__title">
              {detail ? `${detail.kind} · ${detail.name}` : "매니페스트"}
            </h2>
            {detail && (
              <p className="resource-drawer__meta muted">
                {detail.namespace ? `${detail.namespace} · ` : ""}
                {detail.apiVersion}
              </p>
            )}
          </div>
          <button ref={closeRef} type="button" className="linkish" onClick={onClose}>
            닫기
          </button>
        </div>

        {loading ? (
          <div className="state" aria-busy="true">
            <span className="state__title">매니페스트를 불러오는 중…</span>
          </div>
        ) : message || !detail ? (
          <div className="state state--error" role="alert">
            <span className="state__glyph" aria-hidden="true">
              ✕
            </span>
            <span className="state__title">매니페스트를 불러오지 못했습니다</span>
            <span className="state__detail">{message ?? "잠시 후 다시 시도하세요."}</span>
          </div>
        ) : (
          <>
            <dl className="resource-drawer__facts">
              <div>
                <dt>UID</dt>
                <dd className="ds-ident">{detail.uid}</dd>
              </div>
              <div>
                <dt>resourceVersion</dt>
                <dd className="ds-ident">{detail.resourceVersion || "-"}</dd>
              </div>
            </dl>

            <p className="resource-drawer__redaction" role="note">
              <span className="resource-drawer__redaction-mark" aria-hidden="true">
                !
              </span>
              서버가 정제한 읽기 전용 매니페스트입니다. Secret의 <code>data</code>/<code>stringData</code>와
              서버 관리 필드·민감 annotation은 <strong>서버에서 제거된 뒤</strong> 전달됩니다. 원문 조회 경로는 없습니다.
              {detail.redacted && detail.redacted.length > 0 && (
                <>
                  {" "}
                  제거된 항목: <span className="ds-ident">{detail.redacted.join(", ")}</span>
                </>
              )}
            </p>

            <pre className="resource-drawer__yaml" tabIndex={0} aria-label={`${detail.name} 매니페스트 (YAML, 읽기 전용)`}>
              {detail.yaml}
            </pre>
          </>
        )}
      </div>
    </div>
  );
}
