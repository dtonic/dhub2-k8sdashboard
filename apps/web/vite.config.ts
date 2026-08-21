import { defineConfig } from "vitest/config";
import react from "@vitejs/plugin-react";
import { fileURLToPath, URL } from "node:url";

export default defineConfig(({ command, mode }) => ({
  plugins: [react()],
  /* mock worker는 production dist에 넣지 않습니다. 단, 개발 서버(serve)는 MSW 위에서
     돌아가는 것이 기본이므로 public/을 항상 서빙합니다 — 끄면 mock worker 404로
     `make dev`가 빈 화면이 됩니다. build는 명시적 E2E 번들만 public을 복사합니다. */
  publicDir: command === "serve" || mode === "e2e" ? "public" : false,
  resolve: {
    alias: {
      "@": fileURLToPath(new URL("./src", import.meta.url)),
      "@k8s-dashboard/contracts": fileURLToPath(new URL("../../packages/contracts/src/index.ts", import.meta.url)),
      "@k8s-dashboard/dashboard-schema": fileURLToPath(new URL("../../packages/dashboard-schema/src/index.js", import.meta.url)),
      "@k8s-dashboard/design-system": fileURLToPath(new URL("../../design-system", import.meta.url)),
    },
  },
  server: { port: 5173 },
  build: { outDir: "dist", sourcemap: true },
  test: {
    environment: "jsdom",
    setupFiles: "./src/test/setup.ts",
    include: ["src/**/*.test.{ts,tsx}"],
    restoreMocks: true,
  },
  /* 기존 MSW E2E는 OS 환경 변수 전달에 기대지 않는 전용 mode로만 mock을 켭니다. */
  define: mode === "e2e" ? { "import.meta.env.VITE_USE_MOCK": JSON.stringify("true") } : undefined,
}));
