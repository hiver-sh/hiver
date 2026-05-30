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
  build: {
    lib: {
      entry: path.resolve(__dirname, "src/index.ts"),
      fileName: "index",
      formats: ["es"],
    },
    outDir: "dist/lib",
    rollupOptions: {
      external: ["react", "react/jsx-runtime", "react-dom", "react-router-dom"],
    },
  },
});
