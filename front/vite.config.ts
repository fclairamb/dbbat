import path from "path";
import { fileURLToPath } from "url";
import tailwindcss from "@tailwindcss/vite";
import { defineConfig } from "vite";
import react from "@vitejs/plugin-react-swc";
import { TanStackRouterVite } from "@tanstack/router-plugin/vite";

const __dirname = path.dirname(fileURLToPath(import.meta.url));

// Base URL can be configured via VITE_BASE_URL env var
// Default is "/app/" for both dev and production
const getBaseUrl = () => {
  const envBase = process.env.VITE_BASE_URL;
  if (envBase) {
    // Ensure it ends with "/" for Vite
    return envBase.endsWith("/") ? envBase : envBase + "/";
  }
  return "/app/";
};

export default defineConfig(({ command }) => {
  const base = getBaseUrl();

  return {
    plugins: [TanStackRouterVite(), react(), tailwindcss()],
    resolve: {
      alias: {
        "@": path.resolve(__dirname, "./src"),
      },
    },
    base,
    define: {
      // Expose base URL to the app for router configuration
      "import.meta.env.VITE_BASE_URL": JSON.stringify(base.replace(/\/$/, "")),
      // In development, use the proxy at /api instead of direct connection
      // to avoid CORS issues
    },
    server: {
      proxy: {
        "/api": {
          target: "http://localhost:8080",
          changeOrigin: true,
        },
      },
    },
  };
});
