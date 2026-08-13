import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { BrowserRouter, Navigate, Route, Routes } from "react-router-dom";
import { AppShell } from "@/app/AppShell";
import { Placeholder } from "@/app/Placeholder";
import { ClusterOverview } from "@/features/overview/ClusterOverview";
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
              <Route path="/namespaces" element={<Placeholder title="Namespaces" issue="이슈 #15" />} />
              <Route path="/namespaces/:namespace" element={<Placeholder title="Namespace 상세" issue="이슈 #15" />} />
              <Route path="/workloads/:name" element={<Placeholder title="Workload 상세" issue="이슈 #15" />} />
              <Route path="/pods/:name" element={<Placeholder title="Pod 상세" issue="이슈 #15" />} />
              <Route path="/topology" element={<Placeholder title="Pod Topology" issue="이슈 #16" />} />
              <Route path="/logs" element={<Placeholder title="Logs Explorer" issue="이슈 #16" />} />
              <Route path="/alerts" element={<Placeholder title="Alerts" issue="이슈 #17" />} />
              <Route path="*" element={<Navigate to="/" replace />} />
            </Route>
          </Routes>
        </BrowserRouter>
      </QueryClientProvider>
    </StrictMode>,
  );
}

void bootstrap();
