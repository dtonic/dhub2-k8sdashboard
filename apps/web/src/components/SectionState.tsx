import type { ReactNode } from "react";
import type { Section, SectionStatus } from "@k8s-dashboard/contracts";

/**
 * 공통 상태 (이슈 #13 / #14 완료 기준)
 * --------------------------------------------------------------------------
 * "데이터 없음 · 권한 없음 · upstream 장애"는 서로 **다르게** 보여야 합니다.
 * 세 경우를 같은 회색 빈 화면으로 처리하면 운영자가 원인을 오판합니다.
 *
 * - loading   : 골격. 실제 높이를 미리 차지해 값이 도착해도 레이아웃이 흔들리지 않습니다.
 * - empty     : 조회는 성공했고 결과가 0건. 보통 좋은 소식입니다.
 * - forbidden : 권한 부족. 필요한 역할을 문구로 알려줍니다.
 * - degraded  : 데이터소스 장애. 마지막 값이 있으면 stale 표시와 함께 계속 보여줍니다.
 * - error     : 화면 단위 실패. 재시도 버튼을 답니다.
 */

export function LoadingState({ lines = 3, height = 160 }: { lines?: number; height?: number }) {
  return (
    <div className="state" style={{ minHeight: height, alignItems: "stretch", justifyContent: "flex-start" }}>
      <span className="skeleton" style={{ height: 14, width: "38%" }} />
      {Array.from({ length: lines }).map((_, i) => (
        <span key={i} className="skeleton" style={{ height: 12, width: `${92 - i * 11}%` }} />
      ))}
      <span className="visually-hidden" role="status">
        불러오는 중
      </span>
    </div>
  );
}

export function EmptyState({ title, detail }: { title: string; detail?: string }) {
  return (
    <div className="state">
      <span className="state__glyph" aria-hidden="true">
        ✓
      </span>
      <span className="state__title">{title}</span>
      {detail && <span className="state__detail">{detail}</span>}
    </div>
  );
}

export function ForbiddenState({ detail }: { detail?: string }) {
  return (
    <div className="state state--forbidden">
      <span className="state__glyph" aria-hidden="true">
        !
      </span>
      <span className="state__title">권한이 없습니다</span>
      <span className="state__detail">
        {detail ?? "이 데이터를 보려면 추가 권한이 필요합니다."} 데이터가 없는 것이 아니라 조회가 거절되었습니다.
      </span>
    </div>
  );
}

export function ErrorState({ detail, onRetry }: { detail?: string; onRetry?: () => void }) {
  return (
    <div className="state state--error">
      <span className="state__glyph" aria-hidden="true">
        ✕
      </span>
      <span className="state__title">불러오지 못했습니다</span>
      {detail && <span className="state__detail">{detail}</span>}
      {onRetry && (
        <button type="button" className="linkish" onClick={onRetry}>
          다시 시도
        </button>
      )}
    </div>
  );
}

export function DegradedState({ source, detail }: { source?: string; detail?: string }) {
  return (
    <div className="state state--degraded">
      <span className="state__glyph" aria-hidden="true">
        ▲
      </span>
      <span className="state__title">{source ? `${source} 응답 없음` : "일부 데이터소스 장애"}</span>
      <span className="state__detail">{detail ?? "다른 패널은 계속 동작합니다."}</span>
    </div>
  );
}

const SOURCE_LABEL: Record<string, string> = {
  greptimedb: "GreptimeDB",
  quickwit: "Quickwit",
  kubernetes: "Kubernetes API",
  alertmanager: "Alertmanager",
};

/**
 * Section<T> 봉투를 상태별로 렌더링합니다.
 * degraded인데 마지막 값이 있으면 값을 그리고, 없을 때만 상태 화면을 그립니다.
 */
export function SectionView<T>({
  section,
  loading,
  emptyTitle,
  emptyDetail,
  children,
}: {
  section: Section<T> | undefined;
  loading: boolean;
  emptyTitle: string;
  emptyDetail?: string;
  children: (data: T, status: SectionStatus) => ReactNode;
}) {
  if (loading || !section) return <LoadingState />;
  if (section.status === "forbidden") return <ForbiddenState detail={section.reason} />;
  if (section.status === "empty") return <EmptyState title={emptyTitle} detail={emptyDetail} />;
  if (section.status === "degraded" && section.data === undefined) {
    return <DegradedState source={section.source ? SOURCE_LABEL[section.source] : undefined} detail={section.reason} />;
  }
  if (section.data === undefined) return <EmptyState title={emptyTitle} detail={emptyDetail} />;
  return <>{children(section.data, section.status)}</>;
}

export function sourceLabel(source?: string) {
  return source ? (SOURCE_LABEL[source] ?? source) : undefined;
}
