import fs from "fs";
import react from "@vitejs/plugin-react";
import path from "path";
import { defineConfig, type Plugin } from "vite";

// The lib build (dist/lib) is vendored into consuming apps as a standalone
// package, so it needs its own package.json with an `exports` map. Without it,
// `import "sandbox-inspector-client/style.css"` doesn't resolve and consumers
// have to hand-alias the subpath. Emit it alongside the bundle.
function emitPackageJson(): Plugin {
  return {
    name: "emit-lib-package-json",
    apply: "build",
    closeBundle() {
      const pkg = {
        name: "sandbox-inspector-client",
        version: "0.0.1",
        type: "module",
        main: "./index.js",
        module: "./index.js",
        types: "./index.d.ts",
        exports: {
          ".": {
            types: "./index.d.ts",
            import: "./index.js",
          },
          "./style.css": "./index.css",
        },
      };
      const outDir = path.resolve(__dirname, "dist/lib");
      fs.mkdirSync(outDir, { recursive: true });
      fs.writeFileSync(
        path.join(outDir, "package.json"),
        JSON.stringify(pkg, null, 2) + "\n"
      );
    },
  };
}

export default defineConfig({
  plugins: [react(), emitPackageJson()],
  resolve: {
    alias: {
      "@": path.resolve(__dirname, "./src"),
    },
  },
  build: {
    lib: {
      entry: path.resolve(__dirname, "src/lib-entry.ts"),
      fileName: "index",
      formats: ["es"],
    },
    outDir: "dist/lib",
    sourcemap: true,
    target: "es2022",
    rollupOptions: {
      external: ["react", "react/jsx-runtime", "react-dom", "react-router-dom"],
    },
  },
});
