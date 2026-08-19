import { useEffect, useMemo } from "react";
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

/* 화면을 넘어가도 유지할 공용 파라미터 — Scope와 시간 범위뿐입니다.
   from/to·edge·uid·workload·container·levels·q·tab 같은 **화면 전용 필터**는
   nav로 다른 화면에 새어들어가면 안 됩니다. 예전에는 search를 통째로 넘겨
   Topology의 edge나 로그 차트 구간(from/to)이 Logs Explorer로 흘러들어가
   빈 결과("데이터 없음")와 지저분한 URL을 만들었습니다. (#31 후속) */
const SHARED_PARAMS = ["cluster", "ns", "range", "refresh"];
/* 기본값과 같으면 URL에서 생략합니다 — 링크 하나로 재현되는 것은 그대로면서
   "그냥 로그 보기"의 URL이 ?cluster=…&ns=… 수준으로 짧아집니다. */
const PARAM_DEFAULTS: Record<string, string> = { range: "1h", refresh: "30000" };

function sharedSearch(search: string): string {
  const src = new URLSearchParams(search);
  const out = new URLSearchParams();
  for (const k of SHARED_PARAMS) {
    const v = src.get(k);
    if (v && v !== PARAM_DEFAULTS[k]) out.set(k, v);
  }
  const s = out.toString();
  return s ? `?${s}` : "";
}

/** 좌측 내비게이션은 현재 Scope/시간 범위만 유지한 채 이동합니다. */
export function AppShell() {
  const { search } = useLocation();
  const navSearch = useMemo(() => sharedSearch(search), [search]);
  const scope = useScope();
  const canManage = scope.data?.canManageWorkloads ?? false;
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
            <NavLink key={n.to} to={{ pathname: n.to, search: navSearch }} end={n.end} className="app__nav-link">
              {n.label}
            </NavLink>
          ))}
          {embeddedDashboards.map((dashboard) => (
            <NavLink key={dashboard.id} to={{ pathname: `/dashboards/${dashboard.id}`, search: navSearch }} className="app__nav-link">
              {dashboard.title}
            </NavLink>
          ))}
          <NavLink to={{ pathname: "/dashboard-builder", search: navSearch }} className="app__nav-link">Dashboard Builder</NavLink>
        </div>
        {canManage && (
          <div className="app__nav-group">
            <div className="app__nav-title">관리</div>
            <NavLink to={{ pathname: "/deployments", search: navSearch }} className="app__nav-link">Deployments</NavLink>
            <NavLink to={{ pathname: "/secrets", search: navSearch }} className="app__nav-link">Secrets</NavLink>
          </div>
        )}
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
