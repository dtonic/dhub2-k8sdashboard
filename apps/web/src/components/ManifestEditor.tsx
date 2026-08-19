import { useEffect, useMemo, useState } from "react";
import { dump as yamlDump, load as yamlLoad } from "js-yaml";

/**
 * Deployment 매니페스트를 yaml/json 토글로 보고 수정합니다. (#33)
 * 입력은 서버가 준 JSON 문자열이며, 화면 표기 형식만 바꿉니다. 저장 시에는 항상
 * JSON 문자열로 되돌려 상위에 전달합니다(서버 계약은 JSON).
 */
export function ManifestEditor({
  jsonText,
  editable,
  onChange,
}: {
  jsonText: string;
  editable: boolean;
  onChange?: (json: string) => void;
}) {
  const [format, setFormat] = useState<"yaml" | "json">("yaml");
  const [text, setText] = useState("");
  const [error, setError] = useState<string | null>(null);

  // 원본(JSON)이 바뀌면 현재 형식으로 다시 렌더합니다.
  const parsed = useMemo(() => {
    try {
      return JSON.parse(jsonText || "{}");
    } catch {
      return null;
    }
  }, [jsonText]);

  useEffect(() => {
    if (parsed === null) {
      setText(jsonText);
      return;
    }
    setText(format === "yaml" ? yamlDump(parsed, { indent: 2, lineWidth: 120 }) : JSON.stringify(parsed, null, 2));
    setError(null);
    // eslint-disable-next-line react-hooks/exhaustive-deps -- jsonText 변화와 format 변화 모두에 반응합니다.
  }, [jsonText, format]);

  const commit = (next: string) => {
    setText(next);
    if (!onChange) return;
    try {
      const obj = format === "yaml" ? yamlLoad(next) : JSON.parse(next);
      onChange(JSON.stringify(obj));
      setError(null);
    } catch (e) {
      setError(`${format.toUpperCase()} 구문 오류: ${(e as Error).message}`);
    }
  };

  return (
    <div className="manifest-editor">
      <div className="manifest-editor__toolbar">
        <div className="chips" role="group" aria-label="표시 형식">
          {(["yaml", "json"] as const).map((f) => (
            <button key={f} type="button" className="chip" aria-pressed={format === f} onClick={() => setFormat(f)}>
              {f.toUpperCase()}
            </button>
          ))}
        </div>
        {error && (
          <span className="manifest-editor__error" role="alert">
            {error}
          </span>
        )}
      </div>
      <textarea
        className="manifest-editor__area"
        value={text}
        readOnly={!editable}
        spellCheck={false}
        onChange={(e) => commit(e.target.value)}
        aria-label="매니페스트"
      />
    </div>
  );
}
