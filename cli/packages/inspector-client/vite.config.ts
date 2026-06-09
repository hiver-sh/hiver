import react from "@vitejs/plugin-react";
import path from "path";
import { defineConfig } from "vite";

export default defineConfig({
  plugins: [react()],
  resolve: {
    alias: {
      "@": path.resolve(__dirname, "./src"),
    },
  },
  server: {
    port: 5173,
  },
  build: {
    // Minify with terser but DON'T mangle identifiers. esbuild's default
    // identifier minification miscompiles @xterm/xterm's DECRQM handler
    // (`requestMode`), producing `ReferenceError: t is not defined` the moment
    // a full-screen TUI like copilot emits a `CSI ? … $ p` mode query — which
    // only shows in the built bundle, never in `npm run dev` (unminified).
    // Disabling mangling keeps whitespace/dead-code minification while leaving
    // names intact, so xterm runs correctly in production.
    minify: "terser",
    terserOptions: {
      mangle: false,
    },
  },
});
