import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { BrowserRouter, Navigate, Route, Routes } from "react-router-dom";
import { AppShell } from "@/app/AppShell";
import { ClusterOverview } from "@/features/overview/ClusterOverview";
import { NamespaceList } from "@/features/drill/NamespaceList";
import { NamespaceDetail } from "@/features/drill/NamespaceDetail";
import { WorkloadDetail } from "@/features/drill/WorkloadDetail";
import { PodDetail } from "@/features/drill/PodDetail";
import { LogsExplorer } from "@/features/logs/LogsExplorer";
import { TopologyView } from "@/features/topology/TopologyView";
import { AlertsView } from "@/features/alerts/AlertsView";
import { DashboardView } from "@/features/dashboards/DashboardView";
import { DashboardBuilderEditor, DashboardBuilderList } from "@/features/dashboard-builder/DashboardBuilder";
import "./styles/index.css";

const queryClient = new QueryClient({
  defaultOptions: {
    queries: {
      /* 서버가 쿼리 비용을 강제하므로 클라이언트는 과도하게 재요청하지 않습니다. */
      refetchOnWindowFocus: false,
      retry: 1,
      gcTime: 5 * 60 * 1000,
    },
  },
});

async function bootstrap() {
  // WSL→Windows npm 경계에서 인라인 환경 변수가 유실되어도 프로덕션은 mock 없이 빌드됩니다.
  // 프로덕션 mock은 명시적 true에서만 켜고, 개발 서버는 기존 기본값을 유지합니다.
  const useMock = import.meta.env.VITE_USE_MOCK === "true" || (import.meta.env.DEV && import.meta.env.VITE_USE_MOCK !== "false");
  if (useMock) {
    const { startMockApi } = await import("./mocks/browser");
    await startMockApi();
  }

  createRoot(document.getElementById("root")!).render(
    <StrictMode>
      <QueryClientProvider client={queryClient}>
        <BrowserRouter>
          <Routes>
            <Route element={<AppShell />}>
              <Route path="/" element={<ClusterOverview />} />
              <Route path="/namespaces" element={<NamespaceList />} />
              <Route path="/namespaces/:namespace" element={<NamespaceDetail />} />
              <Route path="/workloads/:kind/:name" element={<WorkloadDetail />} />
              <Route path="/pods/:name" element={<PodDetail />} />
              <Route path="/topology" element={<TopologyView />} />
              <Route path="/logs" element={<LogsExplorer />} />
              <Route path="/alerts" element={<AlertsView />} />
              <Route path="/dashboards/:id" element={<DashboardView />} />
              <Route path="/dashboard-builder" element={<DashboardBuilderList />} />
              <Route path="/dashboard-builder/:id" element={<DashboardBuilderEditor />} />
              <Route path="*" element={<Navigate to="/" replace />} />
            </Route>
          </Routes>
        </BrowserRouter>
      </QueryClientProvider>
    </StrictMode>,
  );
}

void bootstrap();
