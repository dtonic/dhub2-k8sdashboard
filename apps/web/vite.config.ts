import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";
import { fileURLToPath, URL } from "node:url";

export default defineConfig({
  plugins: [react()],
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
});
