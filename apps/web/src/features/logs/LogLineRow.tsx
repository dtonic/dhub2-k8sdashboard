import { Link } from "react-router-dom";
import { MASK_LABEL, type LogLine } from "@k8s-dashboard/contracts";
import { withSearch } from "@/components/drill";
import { clock } from "@/lib/format";

/**
 * 로그 한 줄.
 * --------------------------------------------------------------------------
 * 마스킹 정책
 * - 서버가 이미 가린 문자열만 받습니다. 원문은 브라우저에 오지 않습니다.
 * - 가려진 구간은 시각적으로 표시하고, 무엇이 가려졌는지 종류를 알려줍니다.
 * - **복사는 화면에 보이는 것만** 복사합니다. 원문 복구 경로를 만들지 않습니다.
 *   (README §10 — 로그 내 Token/Password/Secret 마스킹)
 */
export function LogLineRow({
  line,
  search,
  open,
  onToggle,
}: {
  line: LogLine;
  search: string;
  /** 펼침 상태는 목록이 관리합니다. 가상 스크롤이 높이를 알아야 하기 때문입니다. */
  open: boolean;
  onToggle: () => void;
}) {
  const long = line.message.length > 160;
  const kinds = [...new Set(line.masked.map((m) => m.kind))];

  return (
    <div className={`logline logline--${line.level.toLowerCase()}`}>
      <button
        type="button"
        className="logline__main"
        aria-expanded={open}
        onClick={onToggle}
        title={open ? "접기" : "펼치기"}
      >
        <span className="logline__time num">{clock(line.t)}</span>
        <span className="logline__level">{line.level}</span>
        <span className={`logline__msg${open || !long ? "" : " logline__msg--clamp"}`}>
          <MaskedMessage line={line} />
        </span>
        {kinds.length > 0 && (
          <span className="logline__masked" title="서버에서 마스킹된 값입니다. 원문은 조회할 수 없습니다.">
            {kinds.map((k) => MASK_LABEL[k]).join("·")} 마스킹됨
          </span>
        )}
      </button>

      {open && (
        <div className="logline__meta">
          <dl className="logline__fields">
            <div>
              <dt>Pod</dt>
              <dd className="ds-ident">
                <Link to={withSearch(`/pods/${encodeURIComponent(line.podName)}`, search, { ns: line.namespace, uid: line.podUid })}>
                  {line.podName}
                </Link>
              </dd>
            </div>
            <div>
              <dt>Pod UID</dt>
              <dd className="ds-ident">{line.podUid}</dd>
            </div>
            <div>
              <dt>Container</dt>
              <dd className="ds-ident">{line.containerName}</dd>
            </div>
            <div>
              <dt>Workload</dt>
              <dd className="ds-ident">
                {line.workloadKind && line.workloadName ? (
                  <Link
                    to={withSearch(`/workloads/${line.workloadKind}/${encodeURIComponent(line.workloadName)}`, search, {
                      ns: line.namespace,
                    })}
                  >
                    {line.workloadName}
                  </Link>
                ) : (
                  "—"
                )}
              </dd>
            </div>
            <div>
              <dt>Node</dt>
              <dd className="ds-ident">{line.nodeName ?? "—"}</dd>
            </div>
            <div>
              <dt>trace / span</dt>
              <dd className="ds-ident">{line.traceId ? `${line.traceId} / ${line.spanId}` : "없음"}</dd>
            </div>
            {Object.entries(line.attributes ?? {}).map(([k, v]) => (
              <div key={k}>
                <dt>{k}</dt>
                <dd className="ds-ident">{v}</dd>
              </div>
            ))}
          </dl>
          <div className="row row--wrap">
            <button
              type="button"
              className="linkish"
              onClick={() => void navigator.clipboard?.writeText(`${new Date(line.t).toISOString()} ${line.level} ${line.message}`)}
            >
              보이는 내용 복사
            </button>
            <span className="muted" style={{ font: "var(--type-meta)" }}>
              {kinds.length
                ? "마스킹된 값은 복사본에도 가려진 상태로 들어갑니다. 원문은 서버 밖으로 나가지 않습니다."
                : "원문 복사 경로는 제공하지 않습니다."}
            </span>
          </div>
        </div>
      )}
    </div>
  );
}

/** 가려진 구간을 시각적으로 구분해 그립니다. 값은 이미 없습니다. */
function MaskedMessage({ line }: { line: LogLine }) {
  if (line.masked.length === 0) return <>{line.message}</>;
  const parts: Array<{ text: string; masked?: string }> = [];
  let cursor = 0;
  for (const m of [...line.masked].sort((a, b) => a.start - b.start)) {
    if (m.start > cursor) parts.push({ text: line.message.slice(cursor, m.start) });
    parts.push({ text: line.message.slice(m.start, m.start + m.length), masked: MASK_LABEL[m.kind] });
    cursor = m.start + m.length;
  }
  if (cursor < line.message.length) parts.push({ text: line.message.slice(cursor) });
  return (
    <>
      {parts.map((p, i) =>
        p.masked ? (
          <span key={i} className="masked" title={`${p.masked} — 서버에서 마스킹됨`}>
            {p.text}
          </span>
        ) : (
          <span key={i}>{p.text}</span>
        ),
      )}
    </>
  );
}
