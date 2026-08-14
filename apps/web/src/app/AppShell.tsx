import { NavLink, Outlet, useLocation } from "react-router-dom";
import { useScope } from "@/api/queries";
import { useDashboardParams } from "@/state/useDashboardParams";
import { embeddedDashboards } from "@/generated/dashboards";

const NAV = [
  { to: "/", label: "Cluster Overview", end: true },
  { to: "/namespaces", label: "Namespaces" },
  { to: "/topology", label: "Pod Topology" },
  { to: "/logs", label: "Logs Explorer" },
  { to: "/alerts", label: "Alerts" },
];

/** 좌측 내비게이션은 현재 Scope/시간 범위를 유지한 채 이동합니다. */
export function AppShell() {
  const { search } = useLocation();
  const scope = useScope();
  const { clusterId } = useDashboardParams();
  const cluster = scope.data?.clusters.find((c) => c.id === clusterId);

  return (
    <div className="app">
      <a className="skip-link" href="#main">
        본문으로 건너뛰기
      </a>

      <div className="app__brand">
        <span className="app__brand-mark" aria-hidden="true" />
        K8s Dashboard
      </div>

      <header className="app__topbar">
        <span className="muted" style={{ font: "var(--type-meta)" }}>
          {cluster ? `${cluster.name} · ${cluster.namespaces === "all" ? "전체 Namespace 접근" : `${cluster.namespaces.length}개 Namespace 접근`}` : " "}
        </span>
        <span className="muted" style={{ font: "var(--type-meta)" }}>
          Mock API · 실데이터 아님
        </span>
      </header>

      <nav className="app__nav" aria-label="주요 화면">
        <div className="app__nav-group">
          <div className="app__nav-title">관측</div>
          {NAV.map((n) => (
            <NavLink key={n.to} to={{ pathname: n.to, search }} end={n.end} className="app__nav-link">
              {n.label}
            </NavLink>
          ))}
          {embeddedDashboards.map((dashboard) => (
            <NavLink key={dashboard.id} to={{ pathname: `/dashboards/${dashboard.id}`, search }} className="app__nav-link">
              {dashboard.title}
            </NavLink>
          ))}
        </div>
      </nav>

      <main className="app__main" id="main">
        <Outlet />
      </main>
    </div>
  );
}
