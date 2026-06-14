import { cpSync, createReadStream, existsSync, statSync } from "node:fs";
import path from "node:path";
import react from "@vitejs/plugin-react";
import { defineConfig, type Plugin } from "vite";

// @ts-expect-error process is a nodejs global
const host = process.env.TAURI_DEV_HOST;

// @grafana/ui's <Icon> fetches SVG sprites at runtime from
// `${__grafana_public_path__}build/img/icons/<subdir>/<name>.svg`. main.tsx
// sets that path to "/", so the icons must be served at /build/img/icons/.
// They ship inside @grafana/ui/dist/public/img/icons — this plugin serves them
// from there in dev and copies them into dist on build, so we don't vendor
// 1100+ SVGs into the repo.
function grafanaIcons(): Plugin {
  // Vite runs from the project root, so resolve assets against cwd (ESM config
  // has no reliable __dirname).
  const src = path.resolve(
    process.cwd(),
    "node_modules/@grafana/ui/dist/public/img/icons"
  );
  const urlPrefix = "/build/img/icons/";
  return {
    name: "grafana-icons",
    configureServer(server) {
      server.middlewares.use((req, res, next) => {
        if (!req.url || !req.url.startsWith(urlPrefix)) return next();
        const rel = req.url.slice(urlPrefix.length).split("?")[0];
        const file = path.join(src, rel);
        if (!file.startsWith(src) || !existsSync(file) || !statSync(file).isFile()) {
          return next();
        }
        res.setHeader("Content-Type", "image/svg+xml");
        createReadStream(file).pipe(res);
      });
    },
    closeBundle() {
      const dest = path.resolve(process.cwd(), "dist/build/img/icons");
      if (existsSync(src)) cpSync(src, dest, { recursive: true });
    },
  };
}

// https://v2.tauri.app/start/frontend/vite/
export default defineConfig(async () => ({
  plugins: [react(), grafanaIcons()],

  // @grafana/ui + @grafana/data ship ESM that re-exports through hashed chunks;
  // rollup's production build can't always follow those named re-exports (e.g.
  // `colorManipulator`). Pre-bundling them with esbuild flattens the re-exports
  // so both dev and build resolve the same way.
  optimizeDeps: {
    include: ["@grafana/data", "@grafana/ui", "react", "react-dom"],
  },
  build: {
    commonjsOptions: {
      include: [/node_modules/],
      transformMixedEsModules: true,
    },
  },

  // Vite options tailored for Tauri development.
  clearScreen: false,
  server: {
    port: 1420,
    strictPort: true,
    host: host || false,
    hmr: host ? { protocol: "ws", host, port: 1421 } : undefined,
    watch: {
      ignored: ["**/src-tauri/**"],
    },
  },
}));
