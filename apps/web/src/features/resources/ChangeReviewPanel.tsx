import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import type {
  ResourceDetailResponse,
  ResourceDryRunChange,
  ResourceDryRunResponse,
} from "@k8s-dashboard/contracts";
import {
  DRY_RUN_ABSOLUTE_MAX_BYTES,
  DRY_RUN_DEPLOY_DEFAULT_BYTES,
  fixedTextFor,
  identityKey,
  isAbortError,
  LOCAL_REJECT_MESSAGES,
  localReject,
  manifestByteLength,
  responseMatchesDetail,
  REVIEW_IDENTITY_MISMATCH_MESSAGE,
  reviewErrorMessage,
  submitDryRun,
} from "./dryrun";

/**
 * 변경 검토 (ADR 0019 Phase 1)
 * --------------------------------------------------------------------------
 * 고친 매니페스트를 **적용하지 않고** 서버에 물어봅니다 — "이걸 적용하면 무엇이
 * 달라지고 무엇이 막히는가". 적용·저장·삭제·생성 컨트롤은 이 화면에 없습니다.
 *
 * raw 매니페스트가 사는 곳은 이 컴포넌트의 상태와 POST 본문뿐입니다. URL·저장소·
 * 쿼리 캐시·로그 어디에도 두지 않고, 탭을 벗어나거나 드로어가 닫히면 언마운트와
 * 함께 사라집니다.
 */

/* 계약 상한과 같은 값입니다. 서버가 더 보낼 수 없지만, 화면은 그것을 믿고
   무한히 그리지 않습니다 — 방어적으로 한 번 더 자릅니다. */
const MAX_CHANGES = 200;
const MAX_WARNINGS = 32;
const MAX_VIOLATIONS = 32;
const MAX_REDACTED = 64;

type Phase = "idle" | "submitting" | "result" | "error";

/* 계약 enum을 화면 문구로 옮기는 표입니다.
   **모르는 값은 그대로 되비추지 않습니다** — 서버가 무엇을 보내든 DOM에 나가는
   문자열은 아래 표 안의 것뿐입니다. 텍스트든 속성이든 같습니다. */

const OUTCOME: Record<string, { glyph: string; label: string; detail: string }> = {
  unchanged: { glyph: "=", label: "변경 없음", detail: "예상 변경이 없습니다." },
  changed: { glyph: "±", label: "변경 있음", detail: "아래가 예상 변경입니다." },
  rejected: { glyph: "!", label: "거절됨", detail: "서버가 이 변경을 받아들이지 않았습니다." },
};

const UNKNOWN_OUTCOME = {
  glyph: "?",
  label: "결과를 해석할 수 없습니다",
  detail: "서버가 이 화면이 모르는 형식으로 답했습니다.",
};

/** data 속성에도 알려진 값 또는 "unknown"만 들어갑니다. */
const UNKNOWN_OUTCOME_ATTR = "unknown";

const REJECTED_BY: Record<string, string> = {
  validation: "서버 검증에서 거절",
  admission: "admission webhook이 거절",
  conflict: "다른 field manager와 소유권 충돌",
};

const UNKNOWN_REJECTED_BY = "알 수 없는 사유";

const OP_LABEL: Record<string, string> = { added: "추가", removed: "제거", changed: "변경" };

const UNKNOWN_OP = "알 수 없음";

export interface ChangeReviewPanelProps {
  /** 상세가 서버에서 받아 온 신원과 정제된 YAML입니다. 편집기의 seed이기도 합니다. */
  detail: ResourceDetailResponse;
}

/**
 * 편집 상태입니다. `draft`는 **언제나 1MiB 이하**입니다.
 *
 * 상한을 넘는 문자열은 상태에 복제하지 않습니다 — 붙여넣은 순간 사본이 생기고,
 * 그 뒤로 렌더마다 바이트를 다시 세게 됩니다. 넘으면 편집기를 비우고 `oversize`
 * 래치를 세워, 다음 유효한 입력이 올 때까지 오류를 유지합니다.
 */
interface EditorState {
  draft: string;
  oversize: boolean;
}

function boundedSeed(yaml: string): EditorState {
  if (manifestByteLength(yaml) > DRY_RUN_ABSOLUTE_MAX_BYTES) return { draft: "", oversize: true };
  return { draft: yaml, oversize: false };
}

export function ChangeReviewPanel({ detail }: ChangeReviewPanelProps) {
  const [editor, setEditor] = useState<EditorState>(() => boundedSeed(detail.yaml));
  const [phase, setPhase] = useState<Phase>("idle");
  const [result, setResult] = useState<ResourceDryRunResponse | undefined>(undefined);
  const [message, setMessage] = useState<string | undefined>(undefined);

  const controllerRef = useRef<AbortController | undefined>(undefined);
  /* genRef는 "이 응답이 아직 유효한 시도인가"를 셉니다. */
  const genRef = useRef(0);
  /* currentIdentityRef는 **렌더 중 동기적으로** 갱신합니다. effect에 미루면 대상이
     바뀐 직후 도착한 응답이 옛 값과 비교되어 통과합니다. */
  const identity = identityKey(detail);
  const currentIdentityRef = useRef(identity);
  currentIdentityRef.current = identity;

  const abortInFlight = useCallback(() => {
    controllerRef.current?.abort();
    controllerRef.current = undefined;
    genRef.current += 1;
  }, []);

  const clearOutcome = useCallback(() => {
    setPhase("idle");
    setResult(undefined);
    setMessage(undefined);
  }, []);

  /* 대상이 바뀌면 진행 중 요청을 끊고 편집·결과를 새 대상에 맞게 되돌립니다. */
  useEffect(() => {
    abortInFlight();
    setEditor(boundedSeed(detail.yaml));
    clearOutcome();
  }, [identity, detail.yaml, abortInFlight, clearOutcome]);

  /* 언마운트(탭 전환·드로어 닫힘)도 취소입니다. */
  useEffect(() => () => controllerRef.current?.abort(), []);

  const onDraftChange = (value: string) => {
    /* 입력이 바뀌면 이전 결과는 더 이상 이 입력의 결과가 아닙니다. */
    abortInFlight();
    clearOutcome();
    if (manifestByteLength(value) > DRY_RUN_ABSOLUTE_MAX_BYTES) {
      /* 큰 문자열을 **상태에 넣지 않습니다.** 넣는 순간 사본이 남습니다. */
      setEditor({ draft: "", oversize: true });
      return;
    }
    setEditor({ draft: value, oversize: false });
  };

  const draft = editor.draft;

  const onReview = async () => {
    if (editor.oversize) {
      /* 상한을 넘겨 비운 상태입니다. 요청을 만들지 않고 거절을 유지합니다. */
      abortInFlight();
      setResult(undefined);
      setMessage(LOCAL_REJECT_MESSAGES.too_large);
      setPhase("error");
      return;
    }
    const reject = localReject(draft);
    if (reject) {
      /* 확실히 거절될 입력은 **요청을 만들지 않습니다.** */
      abortInFlight();
      setResult(undefined);
      setMessage(LOCAL_REJECT_MESSAGES[reject]);
      setPhase("error");
      return;
    }
    abortInFlight();
    const controller = new AbortController();
    controllerRef.current = controller;
    const generation = genRef.current;
    const requestedIdentity = currentIdentityRef.current;
    setResult(undefined);
    setMessage(undefined);
    setPhase("submitting");

    try {
      const response = await submitDryRun({ detail, manifest: draft, signal: controller.signal });
      if (generation !== genRef.current || requestedIdentity !== currentIdentityRef.current) return;
      if (!responseMatchesDetail(response, detail)) {
        /* 다른 대상의 결과를 보여 주는 것보다 아무것도 보여 주지 않는 편이 낫습니다.
           응답 내용은 화면에 넣지 않습니다. */
        setResult(undefined);
        setMessage(REVIEW_IDENTITY_MISMATCH_MESSAGE);
        setPhase("error");
        return;
      }
      setResult(response);
      setPhase("result");
    } catch (error) {
      /* 취소는 사용자가 만든 오류가 아닙니다. */
      if (isAbortError(error, controller.signal)) return;
      if (generation !== genRef.current || requestedIdentity !== currentIdentityRef.current) return;
      setResult(undefined);
      setMessage(reviewErrorMessage(error));
      setPhase("error");
    } finally {
      if (controllerRef.current === controller) controllerRef.current = undefined;
    }
  };

  /* draft는 언제나 1MiB 이하이므로 이 계산은 유계입니다. */
  const bytes = useMemo(() => manifestByteLength(draft), [draft]);
  /* 상한 초과 래치는 phase와 별개로 계속 보입니다 — 다음 유효 입력에서만 풀립니다. */
  const shownError = editor.oversize ? LOCAL_REJECT_MESSAGES.too_large : phase === "error" ? message : undefined;

  return (
    <div className="resource-review">
      <p className="resource-review__disclaimer" role="note">
        <span className="resource-review__glyph" aria-hidden="true">
          i
        </span>
        검토만 하며 클러스터에 적용하지 않습니다. 서버가 dryRun으로 물어보고 결과만 돌려줍니다.
      </p>

      <label className="resource-review__editor" htmlFor="resource-review-manifest">
        <span className="resource-review__label">검토할 매니페스트</span>
        <textarea
          id="resource-review-manifest"
          aria-label={`${detail.name} 매니페스트 (YAML, 검토용 편집)`}
          className="resource-review__textarea"
          value={draft}
          rows={16}
          spellCheck={false}
          autoComplete="off"
          autoCapitalize="off"
          /* 문자 수 기준 보조 장치입니다. UTF-8 바이트 검사를 **대체하지 않습니다** —
             멀티바이트 입력은 이 상한 안에서도 1MiB를 넘길 수 있습니다. */
          maxLength={DRY_RUN_ABSOLUTE_MAX_BYTES}
          onChange={(e) => onDraftChange(e.target.value)}
        />
      </label>

      <div className="resource-review__actions">
        {/* 진행 중에도 비활성화하지 않습니다. 다시 누르면 **이전 요청을 끊고** 새로
            보내는 것이 계약이고, 버튼을 잠그면 그 경로 자체가 닿을 수 없게 됩니다.
            어느 순간에도 살아 있는 요청은 하나뿐입니다. */}
        <button type="button" className="ds-button ds-button--sm" onClick={() => void onReview()}>
          변경 검토
        </button>
        <span className="resource-review__bytes muted">
          {bytes.toLocaleString()}바이트
          {editor.oversize ? " · 1MiB를 넘어 편집기를 비웠습니다" : ""}
          {" · 배포 기본 상한 "}
          {(DRY_RUN_DEPLOY_DEFAULT_BYTES / 1024).toLocaleString()}KiB (서버가 최종 판정)
        </span>
      </div>

      {phase === "idle" && !shownError && (
        <div className="state">
          <span className="state__glyph" aria-hidden="true">
            →
          </span>
          <span className="state__title">아직 검토하지 않았습니다</span>
          <span className="state__detail">매니페스트를 고치고 “변경 검토”를 누르세요.</span>
        </div>
      )}

      {phase === "submitting" && !shownError && (
        <div className="state" aria-busy="true">
          <span className="state__title">검토하는 중…</span>
        </div>
      )}

      {shownError && (
        <div className="state state--error" role="alert">
          <span className="state__glyph" aria-hidden="true">
            ✕
          </span>
          <span className="state__title">검토하지 못했습니다</span>
          <span className="state__detail">{shownError}</span>
        </div>
      )}

      {phase === "result" && result && !editor.oversize && <ReviewResult result={result} />}
    </div>
  );
}

/** 결과 렌더는 계약 상한 안에서만 그립니다. 색만으로 상태를 전달하지 않습니다. */
function ReviewResult({ result }: { result: ResourceDryRunResponse }) {
  /* 모르는 값은 표에 없는 문구로만 답합니다. 원문을 되비추지 않습니다. */
  const known = Object.prototype.hasOwnProperty.call(OUTCOME, result.outcome);
  const outcome = known ? OUTCOME[result.outcome] : UNKNOWN_OUTCOME;
  const outcomeAttr = known ? result.outcome : UNKNOWN_OUTCOME_ATTR;
  const changes = (result.changes ?? []).slice(0, MAX_CHANGES);
  const warnings = (result.warnings ?? []).slice(0, MAX_WARNINGS);
  const violations = (result.violations ?? []).slice(0, MAX_VIOLATIONS);
  const redacted = (result.redacted ?? []).slice(0, MAX_REDACTED);

  return (
    <section className="resource-review__result" aria-label="검토 결과">
      <p className="resource-review__outcome" data-outcome={outcomeAttr}>
        <span className="resource-review__glyph" aria-hidden="true">
          {outcome.glyph}
        </span>
        <strong>{outcome.label}</strong>
        <span className="muted"> {outcome.detail}</span>
        {result.rejectedBy && (
          <span className="resource-review__rejected">
            {" "}
            사유: {fixedTextFor(REJECTED_BY, result.rejectedBy) ?? UNKNOWN_REJECTED_BY}
          </span>
        )}
      </p>

      <p className="resource-review__count muted">
        변경 {result.changeCount}건
        {result.truncated ? ` · 목록은 ${changes.length}건까지만 표시합니다(잘림)` : ""}
      </p>

      {changes.length > 0 && (
        <div className="panel__scroll panel__scroll--fixed">
          <table className="ds-data-table ds-data-table--compact">
            <caption className="visually-hidden">예상 변경 목록</caption>
            <thead>
              <tr>
                <th>경로</th>
                <th className="resource-review__op-heading">종류</th>
                <th>값</th>
              </tr>
            </thead>
            <tbody>
              {changes.map((change) => (
                <tr key={`${change.op}:${change.path}`}>
                  <td className="ds-ident">{change.path}</td>
                  <td>{fixedTextFor(OP_LABEL, change.op) ?? UNKNOWN_OP}</td>
                  <td>
                    <ChangeValue change={change} />
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}

      {violations.length > 0 && (
        <div className="resource-review__list">
          <h3 className="resource-review__list-title">거절 사유 {violations.length}건</h3>
          <ul>
            {violations.map((violation, index) => (
              <li key={`violation-${index}`}>
                <span className="resource-review__glyph" aria-hidden="true">
                  !
                </span>
                {violation.message}
              </li>
            ))}
          </ul>
        </div>
      )}

      {warnings.length > 0 && (
        <div className="resource-review__list">
          <h3 className="resource-review__list-title">서버 경고 {warnings.length}건</h3>
          <ul>
            {warnings.map((warning, index) => (
              <li key={`warning-${index}`}>
                <span className="resource-review__glyph" aria-hidden="true">
                  ▲
                </span>
                {warning}
              </li>
            ))}
          </ul>
        </div>
      )}

      {redacted.length > 0 && (
        <p className="resource-review__redacted muted" role="note">
          비교에서 제외된 경로 {redacted.length}개: <span className="ds-ident">{redacted.join(", ")}</span>
        </p>
      )}
    </section>
  );
}

/**
 * 값 한 칸입니다.
 *
 * `valueRedacted`면 before/after를 **읽지도 않습니다.** 조건부 렌더링으로만 가리면
 * 나중에 누군가 툴팁이나 title 속성에 같은 값을 붙일 자리가 남습니다.
 */
function ChangeValue({ change }: { change: ResourceDryRunChange }) {
  if (change.valueRedacted) {
    return <span className="resource-review__redacted-value">값은 표시하지 않습니다</span>;
  }
  return (
    <span className="resource-review__value">
      <span className="ds-ident">{change.before ?? "-"}</span>
      <span aria-hidden="true"> → </span>
      <span className="visually-hidden">에서</span>
      <span className="ds-ident">{change.after ?? "-"}</span>
      <span className="visually-hidden">(으)로</span>
      {change.valueTruncated && <span className="resource-review__truncated"> (값 잘림)</span>}
    </span>
  );
}
