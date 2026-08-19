import { useCallback, useEffect, useRef, useState, type ReactNode } from "react";

/**
 * 가상 스크롤 테이블 (이슈 #15 완료 기준: Workload 수가 많아도 가상화 적용)
 * --------------------------------------------------------------------------
 * 라이브러리를 붙이기 전이라 최소 구현입니다. 고정 행 높이를 전제로 하고,
 * 보이는 구간 + 위아래 여유분만 렌더링합니다.
 *
 * 중요한 건 성능 수치가 아니라 **갱신 시 스크롤과 선택이 초기화되지 않는 것**입니다.
 * 그래서 스크롤 위치는 DOM(컨테이너 scrollTop)이 그대로 들고 있고,
 * 데이터가 바뀌어도 컨테이너를 다시 만들지 않습니다.
 */
export function VirtualTable<T>({
  items,
  rowHeight = 32,
  height = 420,
  overscan = 8,
  columns,
  header,
  renderRow,
  getKey,
  empty,
}: {
  items: T[];
  rowHeight?: number;
  height?: number;
  overscan?: number;
  /**
   * 컬럼 폭(CSS 값 배열). head와 body는 별개의 <table>이므로, 같은 colgroup을
   * 양쪽에 렌더해야 컬럼 경계가 일치합니다. 없으면 fixed 레이아웃이 균등 분배합니다.
   */
  columns?: string[];
  header: ReactNode;
  renderRow: (item: T, index: number) => ReactNode;
  getKey: (item: T, index: number) => string;
  empty?: ReactNode;
}) {
  const ref = useRef<HTMLDivElement>(null);
  const [scrollTop, setScrollTop] = useState(0);

  const onScroll = useCallback(() => {
    if (ref.current) setScrollTop(ref.current.scrollTop);
  }, []);

  /* 목록이 짧아져 현재 스크롤이 범위를 벗어나면 그때만 보정합니다. */
  useEffect(() => {
    const el = ref.current;
    if (!el) return;
    const max = Math.max(0, items.length * rowHeight - el.clientHeight);
    if (el.scrollTop > max) el.scrollTop = max;
  }, [items.length, rowHeight]);

  if (items.length === 0 && empty) return <>{empty}</>;

  const total = items.length * rowHeight;
  /* 행이 적으면 뷰포트를 줄입니다. 빈 스크롤 영역은 목록이 더 있는 것처럼 보이게 합니다. */
  const viewport = Math.min(height, Math.max(rowHeight * 3, total));
  const start = Math.max(0, Math.floor(scrollTop / rowHeight) - overscan);
  const visible = Math.ceil(viewport / rowHeight) + overscan * 2;
  const end = Math.min(items.length, start + visible);
  const slice = items.slice(start, end);
  const colgroup = columns ? (
    <colgroup>
      {columns.map((w, i) => (
        <col key={i} style={{ width: w }} />
      ))}
    </colgroup>
  ) : null;

  return (
    <div className="vtable">
      <table className="ds-data-table ds-data-table--compact vtable__head">
        {colgroup}
        <thead>{header}</thead>
      </table>
      <div className="vtable__viewport" ref={ref} onScroll={onScroll} style={{ height: viewport }} tabIndex={0}>
        <div style={{ height: total, position: "relative" }}>
          <table
            className="ds-data-table ds-data-table--compact vtable__body"
            style={{ position: "absolute", top: start * rowHeight, left: 0, right: 0 }}
          >
            {colgroup}
            <tbody>
              {slice.map((item, i) => (
                <tr key={getKey(item, start + i)} style={{ height: rowHeight }}>
                  {renderRow(item, start + i)}
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      </div>
      <div className="vtable__footer">
        전체 <span className="num">{items.length.toLocaleString("ko-KR")}</span>행 · 화면에 렌더된 행{" "}
        <span className="num">{slice.length}</span>
      </div>
    </div>
  );
}
