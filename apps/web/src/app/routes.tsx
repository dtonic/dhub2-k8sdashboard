import type { ReactElement } from "react";
import { ClusterOverview } from "@/features/overview/ClusterOverview";
import { NodesView } from "@/features/nodes/NodesView";
import { NamespaceList } from "@/features/drill/NamespaceList";
import { NamespaceDetail } from "@/features/drill/NamespaceDetail";
import { WorkloadDetail } from "@/features/drill/WorkloadDetail";
import { PodDetail } from "@/features/drill/PodDetail";
import { LogsExplorer } from "@/features/logs/LogsExplorer";
import { TopologyView } from "@/features/topology/TopologyView";
import { ManageView } from "@/features/manage/ManageView";
import { ResourcesView } from "@/features/resources/ResourcesView";
import { AlertsView } from "@/features/alerts/AlertsView";
import { DashboardView } from "@/features/dashboards/DashboardView";
import { DashboardBuilderEditor, DashboardBuilderList } from "@/features/dashboard-builder/DashboardBuilder";

/**
 * 라우트 레지스트리 (ADR 0023 Phase 1)
 * --------------------------------------------------------------------------
 * **정적 라우트의 단일 원천**입니다. 라우터·좌측 nav·Command Palette가 모두
 * 이 배열 하나를 읽습니다.
 *
 * 예전에는 같은 사실이 세 곳에 흩어져 있었습니다 — `main.tsx`의 `<Route>`,
 * `AppShell`의 `NAV` 리터럴, 그리고 화면마다 손으로 쓴 링크. 하나를 고치고
 * 나머지를 잊으면 "nav에는 있는데 라우터에는 없는 경로"가 생기고, 그 경로는
 * catch-all에 걸려 조용히 홈으로 돌아갑니다. 팔레트가 붙으면 그런 항목이
 * 검색 결과로도 나오므로 더 눈에 띄지 않게 나쁩니다.
 *
 * 규칙
 * - **경로 집합을 바꾸지 않습니다.** 이 파일은 기존 라우트를 옮겨 담은 것이고
 *   추가·삭제·리다이렉트가 없습니다. `/resources`·`/deployments`·`/secrets`는
 *   서로 다른 화면으로 그대로 남습니다.
 * - **capability는 여기서 선언만 합니다.** 실제 권한은 서버가 강제하고
 *   (`/api/v1/scope`), 이 값은 "노출 여부"의 기존 규칙을 그대로 옮긴 것입니다.
 * - catch-all(`*`)만 이 레지스트리 밖에 남습니다 — element가 라우팅 자체가
 *   아니라 리다이렉트라서 목록의 다른 항목과 성격이 다릅니다.
 */

/** 좌측 nav의 그룹. 기존 AppShell의 그룹 순서를 그대로 옮겼습니다. */
export type RouteGroup = "observe" | "resources" | "manage" | "custom";

/**
 * 노출 조건. 이름은 `/api/v1/scope` 응답 필드와 1:1입니다.
 * `always`는 클러스터 접근 권한만 있으면 보이는 화면입니다.
 */
export type RouteCapability = "always" | "canExploreResources" | "canManageWorkloads";

export interface AppRoute {
  /** 안정적인 식별자. 팔레트 결과 key와 테스트가 이 값을 씁니다. */
  id: string;
  /** react-router path. 파라미터 세그먼트를 포함합니다. */
  path: string;
  /** 사람이 읽는 이름. nav 라벨이자 팔레트 검색 대상입니다. */
  label: string;
  group: RouteGroup;
  capability: RouteCapability;
  /**
   * NavLink `end` 의미. 루트(`/`)만 정확 일치가 필요합니다 —
   * 없으면 모든 경로에서 활성으로 보입니다.
   */
  end?: boolean;
  /**
   * 좌측 nav와 팔레트의 이동 목록에 나타날지.
   * 파라미터가 필요한 상세 화면은 목적지를 만들 수 없으므로 false입니다.
   */
  nav: boolean;
  element: ReactElement;
}

export const APP_ROUTES: AppRoute[] = [
  { id: "overview", path: "/", label: "Cluster Overview", group: "observe", capability: "always", end: true, nav: true, element: <ClusterOverview /> },
  { id: "nodes", path: "/nodes", label: "Nodes", group: "observe", capability: "always", nav: true, element: <NodesView /> },
  { id: "namespaces", path: "/namespaces", label: "Namespaces", group: "observe", capability: "always", nav: true, element: <NamespaceList /> },
  { id: "namespace-detail", path: "/namespaces/:namespace", label: "Namespace 상세", group: "observe", capability: "always", nav: false, element: <NamespaceDetail /> },
  { id: "workload-detail", path: "/workloads/:kind/:name", label: "Workload 상세", group: "observe", capability: "always", nav: false, element: <WorkloadDetail /> },
  { id: "pod-detail", path: "/pods/:name", label: "Pod 상세", group: "observe", capability: "always", nav: false, element: <PodDetail /> },
  { id: "topology", path: "/topology", label: "Pod Topology", group: "observe", capability: "always", nav: true, element: <TopologyView /> },
  { id: "logs", path: "/logs", label: "Logs Explorer", group: "observe", capability: "always", nav: true, element: <LogsExplorer /> },
  { id: "alerts", path: "/alerts", label: "Alerts", group: "observe", capability: "always", nav: true, element: <AlertsView /> },

  /* Resources 진입점(ADR 0018). 아래 관리 라우트는 그대로 둡니다 —
     탭에서 이동하는 대상이자 기존 링크·북마크의 목적지입니다. */
  { id: "resources", path: "/resources", label: "Resources", group: "resources", capability: "canExploreResources", nav: true, element: <ResourcesView /> },
  { id: "deployments", path: "/deployments", label: "Deployments", group: "manage", capability: "canManageWorkloads", nav: true, element: <ManageView kind="deployments" /> },
  { id: "secrets", path: "/secrets", label: "Secrets", group: "manage", capability: "canManageWorkloads", nav: true, element: <ManageView kind="secrets" /> },

  { id: "dashboard", path: "/dashboards/:id", label: "Dashboard", group: "custom", capability: "always", nav: false, element: <DashboardView /> },
  { id: "dashboard-builder", path: "/dashboard-builder", label: "Dashboard Builder", group: "custom", capability: "always", nav: true, element: <DashboardBuilderList /> },
  { id: "dashboard-builder-editor", path: "/dashboard-builder/:id", label: "Dashboard Builder 편집", group: "custom", capability: "always", nav: false, element: <DashboardBuilderEditor /> },
];

/** 그룹 표시 순서와 제목. 기존 AppShell의 순서·문구 그대로입니다. */
export const NAV_GROUPS: { id: RouteGroup; title: string }[] = [
  { id: "observe", title: "관측" },
  { id: "resources", title: "리소스" },
  { id: "manage", title: "관리" },
  { id: "custom", title: "Custom" },
];

/** 서버가 준 capability. 이 값이 없으면(로딩 중) 닫힌 쪽으로 답합니다. */
export interface RouteCapabilities {
  canExploreResources: boolean;
  canManageWorkloads: boolean;
}

export function routeAllowed(route: AppRoute, caps: RouteCapabilities): boolean {
  switch (route.capability) {
    case "canExploreResources":
      return caps.canExploreResources;
    case "canManageWorkloads":
      return caps.canManageWorkloads;
    default:
      return true;
  }
}

/** 좌측 nav와 팔레트가 함께 쓰는 목적지 목록. 순서는 레지스트리 순서입니다. */
export function navRoutes(caps: RouteCapabilities): AppRoute[] {
  return APP_ROUTES.filter((r) => r.nav && routeAllowed(r, caps));
}

export function navRoutesInGroup(group: RouteGroup, caps: RouteCapabilities): AppRoute[] {
  return navRoutes(caps).filter((r) => r.group === group);
}
