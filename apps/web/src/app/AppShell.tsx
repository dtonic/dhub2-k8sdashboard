import { useEffect } from "react";
import { NavLink, Outlet, useLocation, useSearchParams } from "react-router-dom";
import { useScope } from "@/api/queries";
import { useDashboardParams } from "@/state/useDashboardParams";
import { embeddedDashboards } from "@/generated/dashboards";
import { useAuth } from "@/app/AuthGate";
import { StreamInvalidator } from "@/app/StreamInvalidator";
import { usingMockApi } from "@/lib/env";

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
  const auth = useAuth();
  const [, setParams] = useSearchParams();

  /* cluster 파라미터가 없으면 scope의 첫 접근 가능 클러스터를 URL에 채웁니다. (#27)
     하드코딩 기본값은 실서버에 존재하지 않는 클러스터로 403을 만들었습니다. */
  useEffect(() => {
    if (clusterId || !scope.data) return;
    const first = scope.data.clusters.find((c) => c.accessible) ?? scope.data.clusters[0];
    if (!first) return;
    setParams(
      (prev) => {
        const p = new URLSearchParams(prev);
        p.set("cluster", first.id);
        return p;
      },
      { replace: true },
    );
  }, [clusterId, scope.data, setParams]);

  return (
    <div className="app">
      <StreamInvalidator clusterId={auth.enabled ? clusterId : undefined} />
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
        {usingMockApi && (
          <span className="muted" style={{ font: "var(--type-meta)" }}>
            Mock API · 실데이터 아님
          </span>
        )}
		{auth.enabled && auth.session?.authenticated && <span className="auth-user">{auth.session.principal.displayName}<button type="button" onClick={() => void auth.logout()}>Sign out</button></span>}
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
          <NavLink to={{ pathname: "/dashboard-builder", search }} className="app__nav-link">Dashboard Builder</NavLink>
        </div>
      </nav>

      <main className="app__main" id="main">
        {clusterId ? (
          <Outlet />
        ) : scope.isError ? (
          <div className="state" role="alert">
            <span className="state__title">클러스터 목록을 불러오지 못했습니다</span>
            <span className="muted">네트워크 또는 인증 상태를 확인한 뒤 새로고침하세요.</span>
          </div>
        ) : (
          <div className="state" aria-busy="true">
            <span className="state__title">클러스터 목록을 불러오는 중…</span>
          </div>
        )}
      </main>
    </div>
  );
}
