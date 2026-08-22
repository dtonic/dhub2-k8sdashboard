/* React의 KeyboardEvent를 그대로 들여오면 아래 focus trap이 쓰는 **DOM**
   KeyboardEvent를 가려 window.addEventListener 시그니처가 어긋납니다. */
import { useEffect, useRef, useState, type KeyboardEvent as ReactKeyboardEvent } from "react";
import type { ResourceDetailResponse } from "@k8s-dashboard/contracts";
import { ChangeReviewPanel } from "./ChangeReviewPanel";
import { identityKey } from "./dryrun";
import { requestErrorMessage } from "./state";

/**
 * 선택한 항목의 상세 (ADR 0018 결정 5·6, ADR 0019 Phase 1)
 * --------------------------------------------------------------------------
 * 기본은 **읽기 전용**입니다. 서버가 정제해 내려준 YAML을 그대로 보여주기만 하고,
 * Secret의 data/stringData는 애초에 응답에 없습니다. 무엇이 제거됐는지는 숨기지
 * 않고 표시합니다 — 가려졌다는 사실 자체가 정보입니다.
 *
 * 서버가 이 GVR에 검토 capability를 준 경우에만 "변경 검토" 서브탭이 생깁니다.
 * capability는 **서버가 준 descriptor.dryRun 하나**에서만 옵니다 — kind·verbs·
 * 사용자 역할로 추론하지 않습니다. 탭은 새 라우트가 아니라 이 드로어 안입니다.
 */

type DrawerTab = "view" | "review";
const TAB_ORDER: DrawerTab[] = ["view", "review"];
const TAB_LABEL: Record<DrawerTab, string> = { view: "매니페스트", review: "변경 검토" };
/* id는 고정 문자열입니다 — 이름·UID 같은 신원을 속성에 싣지 않습니다. */
const TAB_ID: Record<DrawerTab, string> = {
  view: "resource-drawer-tab-view",
  review: "resource-drawer-tab-review",
};
const PANEL_ID: Record<DrawerTab, string> = {
  view: "resource-drawer-panel-view",
  review: "resource-drawer-panel-review",
};

export function ResourceDetailDrawer({
  open,
  loading,
  detail,
  error,
  onClose,
  canReview = false,
}: {
  open: boolean;
  loading: boolean;
  detail: ResourceDetailResponse | undefined;
  error: unknown;
  onClose: () => void;
  /** 서버가 준 descriptor.dryRun. 기본은 false이며 화면이 추론하지 않습니다. */
  canReview?: boolean;
}) {
  const closeRef = useRef<HTMLButtonElement>(null);
  const dialogRef = useRef<HTMLDivElement>(null);
  const [tab, setTab] = useState<DrawerTab>("view");
  const tabRefs = useRef<Record<DrawerTab, HTMLButtonElement | null>>({ view: null, review: null });

  /* 대상 신원은 **전부** 봅니다. uid+resourceVersion만 보면 클러스터·GVR·namespace가
     바뀌었는데 우연히 같은 uid를 만난 경우를 놓칩니다. 검토 패널과 같은 키를 씁니다. */
  const identity = detail ? identityKey(detail) : "";

  /* 기본은 언제나 View입니다. 다시 열거나, 대상이 바뀌거나, capability가 사라지면
     되돌아갑니다 — 그때 검토 패널이 언마운트되면서 편집 초안과 진행 중 요청도
     함께 사라집니다. */
  useEffect(() => {
    setTab("view");
  }, [open, canReview, identity]);

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

  /* 화살표·Home·End는 포커스와 선택을 **함께** 옮깁니다(WAI-ARIA tabs).
     객체 조회 대신 switch를 씁니다 — `moves["constructor"]` 같은 프로토타입 키가
     함수를 돌려주면 undefined 검사를 지나 인덱스로 쓰이게 됩니다. */
  const onTabKeyDown = (event: ReactKeyboardEvent<HTMLButtonElement>, index: number) => {
    let next: number | undefined;
    switch (event.key) {
      case "ArrowRight":
        next = (index + 1) % TAB_ORDER.length;
        break;
      case "ArrowLeft":
        next = (index - 1 + TAB_ORDER.length) % TAB_ORDER.length;
        break;
      case "Home":
        next = 0;
        break;
      case "End":
        next = TAB_ORDER.length - 1;
        break;
      default:
        return;
    }
    event.preventDefault();
    /* 화살표 이동이 드로어 밖으로 새지 않게 여기서 멈춥니다. */
    event.stopPropagation();
    const target = TAB_ORDER[next];
    setTab(target);
    tabRefs.current[target]?.focus();
  };

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
        ) : !canReview ? (
          /* capability가 없으면 오늘의 화면 그대로입니다 — 탭도 편집기도 없습니다. */
          <ReadOnlyManifest detail={detail} />
        ) : (
          <>
            <div className="chips resource-drawer__tabs" role="tablist" aria-label="상세 보기 방식">
              {TAB_ORDER.map((id, index) => (
                <button
                  key={id}
                  ref={(el) => {
                    tabRefs.current[id] = el;
                  }}
                  type="button"
                  role="tab"
                  id={TAB_ID[id]}
                  className="chip"
                  aria-selected={tab === id}
                  aria-controls={PANEL_ID[id]}
                  tabIndex={tab === id ? 0 : -1}
                  onClick={() => setTab(id)}
                  onKeyDown={(e) => onTabKeyDown(e, index)}
                >
                  {TAB_LABEL[id]}
                </button>
              ))}
            </div>

            {/* 활성 탭만 마운트합니다. 숨겨 두면 편집기가 DOM에 남아 초안과 진행 중
                요청이 살아 있고, focus trap이 보이지 않는 요소를 잡습니다. */}
            {tab === "view" ? (
              <div id={PANEL_ID.view} role="tabpanel" aria-labelledby={TAB_ID.view} tabIndex={-1}>
                <ReadOnlyManifest detail={detail} />
              </div>
            ) : (
              <div id={PANEL_ID.review} role="tabpanel" aria-labelledby={TAB_ID.review} tabIndex={-1}>
                {/* key가 신원입니다. 없으면 대상이 바뀐 렌더에서 **같은 인스턴스**가
                    새 detail을 받고, 리셋 effect가 돌기 전까지 이전 초안과 진행 중
                    요청이 새 대상에 붙어 있게 됩니다. key를 두면 그 창 자체가
                    사라집니다 — 옛 인스턴스가 먼저 언마운트되고 새 인스턴스가 섭니다. */}
                <ChangeReviewPanel key={identity} detail={detail} />
              </div>
            )}
          </>
        )}
      </div>
    </div>
  );
}

/**
 * 정제된 읽기 전용 매니페스트입니다.
 *
 * 탭이 있든 없든 **같은 마크업**을 씁니다 — 두 벌로 나누면 한쪽만 고쳐져 capability
 * 유무에 따라 다른 정보를 보여 주게 됩니다.
 */
function ReadOnlyManifest({ detail }: { detail: ResourceDetailResponse }) {
  return (
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
  );
}
