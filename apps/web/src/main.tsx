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
  if (import.meta.env.VITE_USE_MOCK !== "false") {
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
              <Route path="*" element={<Navigate to="/" replace />} />
            </Route>
          </Routes>
        </BrowserRouter>
      </QueryClientProvider>
    </StrictMode>,
  );
}

void bootstrap();
