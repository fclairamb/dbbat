import fs from "fs";
import path from "path";
import { fileURLToPath } from "url";
import tailwindcss from "@tailwindcss/vite";
import { defineConfig, type Plugin } from "vite";
import react from "@vitejs/plugin-react-swc";
import { TanStackRouterVite } from "@tanstack/router-plugin/vite";

const __dirname = path.dirname(fileURLToPath(import.meta.url));

// Packages whose *loaded* code decides what `src/routeTree.gen.ts` looks like.
const ROUTER_CODEGEN_PACKAGES = [
  "@tanstack/router-generator",
  "@tanstack/router-plugin",
];

const installedVersions = (): Record<string, string> => {
  const versions: Record<string, string> = {};
  for (const pkg of ROUTER_CODEGEN_PACKAGES) {
    try {
      const manifest = path.resolve(__dirname, "node_modules", pkg, "package.json");
      versions[pkg] = JSON.parse(fs.readFileSync(manifest, "utf8")).version;
    } catch {
      // Not resolvable (odd install layout) — that package just opts out of the check.
    }
  }
  return versions;
};

// The route tree is generated code that is committed, and CI fails the frontend
// job when a build leaves it modified. That is only sound while the generator is
// a pure function of the route files — which it is, but only from
// @tanstack/router-generator 1.167.25 onwards. Before that its final sort key was
// the route-node object itself, so every node compared equal and the surviving
// order was whatever the parallel `readdir` recursion in `getRouteNodes` happened
// to produce.
//
// A dev server outlives that distinction. Node caches ES modules per process, so
// a `make dev` started before a dependency bump keeps re-emitting the route tree
// with the *old* generator for as long as it runs, while every cold `bun run
// build` uses the new one — and the file flips back and forth by a couple of
// hundred lines on branches that touch no route at all. Restarting the server
// from inside is not a fix: Vite externalises node_modules when it reloads a
// config, so the stale plugin survives `server.restart()`. Only a new process
// picks up the new generator, so all this can do is say so, loudly.
const routerCodegenFreshness = (): Plugin => {
  const bootVersions = installedVersions();
  let timer: ReturnType<typeof setInterval> | undefined;
  let warned = false;

  const check = () => {
    if (warned) return;
    const current = installedVersions();
    const drifted = Object.keys(bootVersions).filter(
      (pkg) => current[pkg] !== undefined && current[pkg] !== bootVersions[pkg],
    );
    if (drifted.length === 0) return;
    warned = true;
    const detail = drifted
      .map((pkg) => `  ${pkg}: running ${bootVersions[pkg]}, installed ${current[pkg]}`)
      .join("\n");
    console.warn(
      `\n\x1b[33m[dbbat] The router codegen packages changed under this dev server:\n${detail}\n` +
        `This process still runs the versions it imported at startup, so it may be\n` +
        `rewriting src/routeTree.gen.ts in an ordering no cold build produces.\n` +
        `Restart it (\`make dev\`) and re-run \`make build-front\` before committing\n` +
        `src/routeTree.gen.ts.\x1b[0m\n`,
    );
  };

  return {
    name: "dbbat:router-codegen-freshness",
    apply: "serve",
    configureServer() {
      check();
      timer = setInterval(check, 30_000);
      timer.unref?.();
    },
    closeBundle() {
      if (timer) clearInterval(timer);
    },
  };
};

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

export default defineConfig(() => {
  const base = getBaseUrl();

  return {
    plugins: [
      TanStackRouterVite(),
      routerCodegenFreshness(),
      react(),
      tailwindcss(),
    ],
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
