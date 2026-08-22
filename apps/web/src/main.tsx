import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { BrowserRouter, Navigate, Route, Routes } from "react-router-dom";
import { AppShell } from "@/app/AppShell";
import { APP_ROUTES } from "@/app/routes";
import "./styles/fonts.css";
import "./styles/index.css";
import { AuthGate } from "@/app/AuthGate";
import { usingMockApi } from "@/lib/env";

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
  if (usingMockApi) {
    const { startMockApi } = await import("./mocks/browser");
    await startMockApi();
  }

  createRoot(document.getElementById("root")!).render(
    <StrictMode>
      <QueryClientProvider client={queryClient}>
        <AuthGate><BrowserRouter>
          <Routes>
            <Route element={<AppShell />}>
              {/* 경로 집합의 단일 원천은 `@/app/routes`입니다. 라우터·좌측 nav·
                  Command Palette가 같은 배열을 읽으므로 셋이 어긋날 수 없습니다.
                  catch-all만 여기 남습니다 — 라우팅이 아니라 리다이렉트입니다. */}
              {APP_ROUTES.map((route) => (
                <Route key={route.id} path={route.path} element={route.element} />
              ))}
              <Route path="*" element={<Navigate to="/" replace />} />
            </Route>
          </Routes>
        </BrowserRouter></AuthGate>
      </QueryClientProvider>
    </StrictMode>,
  );
}

void bootstrap();
