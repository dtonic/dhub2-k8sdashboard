import { Link, useParams, useSearchParams } from "react-router-dom";

/**
 * Drill-down 대상 화면의 자리 표시자입니다.
 * Cluster Overview(#14)의 "2회 이내 클릭" 기준을 실제로 검증하려면 링크가 살아 있어야 하므로,
 * 화면 구현(#15/#16)이 오기 전까지 라우트와 파라미터 전달만 먼저 잡아둡니다.
 */
export function Placeholder({ title, issue }: { title: string; issue: string }) {
  const params = useParams();
  const [search] = useSearchParams();
  const entries = [...Object.entries(params), ...search.entries()].filter(([, v]) => v);

  return (
    <div className="page">
      <header className="page__header">
        <div>
          <h1 className="page__title">{title}</h1>
          <p className="page__subtitle">{issue}에서 구현 예정입니다.</p>
        </div>
      </header>
      <section className="panel">
        <div className="panel__body">
          <div className="state">
            <span className="state__glyph" aria-hidden="true">
              →
            </span>
            <span className="state__title">전달받은 컨텍스트</span>
            <span className="state__detail">
              {entries.length ? entries.map(([k, v]) => `${k}=${v}`).join(" · ") : "없음"}
            </span>
            <Link to="/">Cluster Overview로 돌아가기</Link>
          </div>
        </div>
      </section>
    </div>
  );
}
