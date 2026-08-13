import { useCallback, useEffect, useRef, useState } from "react";
import type { LogLine } from "@k8s-dashboard/contracts";
import { num } from "@/lib/format";
import { LogLineRow } from "./LogLineRow";

const ROW = 26;
/** 펼친 줄의 추가 높이. CSS와 함께 바꿔야 합니다. */
const EXPANDED_EXTRA = 176;

/**
 * 로그 라인 가상 스크롤 + 무한 로딩 (이슈 #16)
 * --------------------------------------------------------------------------
 * - 화면에 보이는 구간만 렌더링합니다. 수천 줄을 한 번에 DOM에 올리지 않습니다.
 * - 바닥에 가까워지면 다음 **커서** 페이지를 요청합니다.
 * - 펼침은 한 번에 한 줄만 허용합니다. 가변 높이를 여러 개 허용하면 오프셋 계산이
 *   추측에 의존하게 되고, 스크롤이 튀는 편이 정보 밀도보다 나쁩니다.
 */
export function LogList({
  lines,
  hasMore,
  loadingMore,
  onLoadMore,
  search,
  height = 520,
}: {
  lines: LogLine[];
  hasMore: boolean;
  loadingMore: boolean;
  onLoadMore: () => void;
  search: string;
  height?: number;
}) {
  const ref = useRef<HTMLDivElement>(null);
  const [scrollTop, setScrollTop] = useState(0);
  const [openId, setOpenId] = useState<string | null>(null);

  const openIndex = openId ? lines.findIndex((l) => l.id === openId) : -1;
  const extraBefore = (i: number) => (openIndex >= 0 && i > openIndex ? EXPANDED_EXTRA : 0);
  const offsetOf = (i: number) => i * ROW + extraBefore(i);
  const total = lines.length * ROW + (openIndex >= 0 ? EXPANDED_EXTRA : 0);

  /** scrollTop → 인덱스. 펼친 줄 이후는 추가 높이를 빼고 계산합니다. */
  const indexAt = (y: number) => {
    if (openIndex < 0) return Math.floor(y / ROW);
    const openEnd = openIndex * ROW + ROW + EXPANDED_EXTRA;
    if (y < openIndex * ROW) return Math.floor(y / ROW);
    if (y < openEnd) return openIndex;
    return Math.floor((y - EXPANDED_EXTRA) / ROW);
  };

  const onScroll = useCallback(() => {
    const el = ref.current;
    if (!el) return;
    setScrollTop(el.scrollTop);
    if (hasMore && !loadingMore && el.scrollHeight - el.scrollTop - el.clientHeight < 320) onLoadMore();
  }, [hasMore, loadingMore, onLoadMore]);

  /* 필터가 바뀌어 목록이 짧아지면 스크롤을 범위 안으로 되돌립니다. */
  useEffect(() => {
    const el = ref.current;
    if (el && el.scrollTop > Math.max(0, total - el.clientHeight)) el.scrollTop = 0;
  }, [total]);

  const overscan = 10;
  const start = Math.max(0, indexAt(scrollTop) - overscan);
  const end = Math.min(lines.length, indexAt(scrollTop + height) + overscan + 1);
  const slice = lines.slice(start, end);

  return (
    <div className="loglist">
      <div className="loglist__viewport" ref={ref} onScroll={onScroll} style={{ height }} tabIndex={0}>
        <div style={{ height: total, position: "relative" }}>
          {slice.map((line, i) => {
            const idx = start + i;
            return (
              <div
                key={line.id}
                style={{ position: "absolute", top: offsetOf(idx), left: 0, right: 0 }}
              >
                <LogLineRow
                  line={line}
                  search={search}
                  open={line.id === openId}
                  onToggle={() => setOpenId((cur) => (cur === line.id ? null : line.id))}
                />
              </div>
            );
          })}
        </div>
      </div>
      <div className="loglist__footer">
        <span>
          불러온 줄 <span className="num">{num(lines.length)}</span> · 화면에 렌더된 줄{" "}
          <span className="num">{slice.length}</span>
        </span>
        <span aria-live="polite">
          {loadingMore ? "다음 페이지 불러오는 중…" : hasMore ? "스크롤하면 계속 불러옵니다" : "마지막 페이지입니다"}
        </span>
      </div>
    </div>
  );
}
