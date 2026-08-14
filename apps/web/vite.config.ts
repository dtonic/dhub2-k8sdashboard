import { defineConfig } from "vitest/config";
import react from "@vitejs/plugin-react";
import { fileURLToPath, URL } from "node:url";

export default defineConfig(({ mode }) => ({
  plugins: [react()],
  /* mock worker는 명시적 E2E bundle에만 복사하며 production dist에는 두지 않습니다. */
  publicDir: mode === "e2e" ? "public" : false,
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
