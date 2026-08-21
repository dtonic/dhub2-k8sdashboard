import { useRef, type KeyboardEvent } from "react";
import { Link, useLocation } from "react-router-dom";
import { useScope } from "@/api/queries";
import { useDashboardParams } from "@/state/useDashboardParams";
import { Breadcrumb, withSearch } from "@/components/drill";
import { PageHeader } from "@/features/drill/common";
import { ResourceExplorer } from "./ResourceExplorer";

/**
 * Resources 진입 화면 (ADR 0018)
 * --------------------------------------------------------------------------
 * 탭 세 개로 리소스 작업의 진입점을 한곳에 모읍니다.
 *
 * - **Explorer** — 이 화면 안에서 렌더링하는 조회 전용 탐색기.
 * - **Deployments / Secrets** — 기존 `/deployments`·`/secrets` 화면으로 이동합니다.
 *   기존 라우트·화면·좌측 nav 항목은 그대로 두고 여기서 **추가 진입점만** 제공합니다.
 *   리다이렉트하지 않으므로 기존 링크와 북마크는 계속 그대로 동작합니다.
 *
 * 탭은 WAI-ARIA tabs 패턴을 따릅니다 — 좌우 화살표·Home/End로 이동하고,
 * 선택 상태는 색이 아니라 `aria-selected`와 텍스트로 전달합니다.
 */

type TabId = "explorer" | "deployments" | "secrets";

const TABS: { id: TabId; label: string; to?: string }[] = [
  { id: "explorer", label: "Explorer" },
  { id: "deployments", label: "Deployments", to: "/deployments" },
  { id: "secrets", label: "Secrets", to: "/secrets" },
];

export function ResourcesView() {
  const { search } = useLocation();
  const { clusterId } = useDashboardParams();
  const scope = useScope();
  const canExplore = scope.data?.canExploreResources ?? false;
  const canManage = scope.data?.canManageWorkloads ?? false;
  const tabRefs = useRef<(HTMLElement | null)[]>([]);

  /* 관리 권한이 없으면 관리 탭은 아예 만들지 않습니다 — 눌러도 403인 탭은 소음입니다.
     기존 nav의 관리 그룹 노출 조건과 같은 값을 씁니다. */
  const tabs = TABS.filter((t) => t.id === "explorer" || canManage);

  const onTabKeyDown = (event: KeyboardEvent, index: number) => {
    const keys: Record<string, number> = {
      ArrowRight: (index + 1) % tabs.length,
      ArrowLeft: (index - 1 + tabs.length) % tabs.length,
      Home: 0,
      End: tabs.length - 1,
    };
    const next = keys[event.key];
    if (next === undefined) return;
    event.preventDefault();
    tabRefs.current[next]?.focus();
  };

  return (
    <div className="page">
      <PageHeader
        title="Resources"
        subtitle={clusterId}
        crumbs={<Breadcrumb items={[{ label: "Cluster Overview", to: withSearch("/", search) }, { label: "Resources" }]} />}
      />

      <div className="chips resource-tabs" role="tablist" aria-label="리소스 작업">
        {tabs.map((tab, index) => {
          const selected = tab.id === "explorer";
          const shared = {
            role: "tab" as const,
            id: `resources-tab-${tab.id}`,
            className: "chip",
            "aria-selected": selected,
            tabIndex: selected ? 0 : -1,
            onKeyDown: (e: KeyboardEvent) => onTabKeyDown(e, index),
          };
          if (!tab.to) {
            return (
              <button
                key={tab.id}
                type="button"
                {...shared}
                aria-controls="resources-panel-explorer"
                ref={(el) => {
                  tabRefs.current[index] = el;
                }}
              >
                {tab.label}
              </button>
            );
          }
          return (
            <Link
              key={tab.id}
              to={{ pathname: tab.to, search }}
              {...shared}
              ref={(el) => {
                tabRefs.current[index] = el;
              }}
            >
              {tab.label}
              <span className="visually-hidden"> — 전용 화면으로 이동</span>
            </Link>
          );
        })}
      </div>

      <div id="resources-panel-explorer" role="tabpanel" aria-labelledby="resources-tab-explorer" tabIndex={-1}>
        {canExplore ? (
          <ResourceExplorer />
        ) : (
          <section className="panel">
            <div className="panel__body">
              <div className="state state--forbidden" role="alert">
                <span className="state__glyph" aria-hidden="true">
                  !
                </span>
                <span className="state__title">리소스 탐색 권한이 없습니다</span>
                <span className="state__detail">
                  이 기능은 platform.admin 전용입니다. 데이터가 없는 것이 아니라 조회가 거절되었습니다.
                </span>
              </div>
            </div>
          </section>
        )}
      </div>
    </div>
  );
}
