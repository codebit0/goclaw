import { defineConfig, loadEnv } from "vite";
import react from "@vitejs/plugin-react";
import tailwindcss from "@tailwindcss/vite";
import path from "path";

export default defineConfig(({ mode }) => {
  const env = loadEnv(mode, process.cwd(), "");
  const backendPort = env.VITE_BACKEND_PORT || "9600";
  const backendHost = env.VITE_BACKEND_HOST || "localhost";

  return {
    plugins: [react(), tailwindcss()],
    resolve: {
      alias: {
        "@": path.resolve(__dirname, "./src"),
      },
    },
    server: {
      port: 5173,
      proxy: {
        "/ws": {
          target: `http://${backendHost}:${backendPort}`,
          ws: true,
          changeOrigin: true,
        },
        "/v1": {
          target: `http://${backendHost}:${backendPort}`,
          changeOrigin: true,
        },
        "/health": {
          target: `http://${backendHost}:${backendPort}`,
          changeOrigin: true,
        },
        // All /browser/* API + WS endpoints proxied to backend.
        // Exception: GET /browser/live/{token} (SPA page) handled by React Router.
        "/browser": {
          target: `http://${backendHost}:${backendPort}`,
          changeOrigin: true,
          ws: true,
          bypass(req) {
            const p = req.url || "";
            // Let React SPA handle the live view HTML page (GET /browser/live/{token} without /ws or /info suffix)
            if (req.method === "GET" && p.match(/^\/browser\/live\/[^/]+$/) && !p.endsWith("/ws") && !p.endsWith("/info")) {
              return req.url;
            }
            return undefined; // proxy everything else
          },
        },
      },
    },
    build: {
      outDir: "dist",
      emptyOutDir: true,
    },
  };
});
