// Worker wiring lives in the app entry ONLY. The published lib bundle
// (consumed e.g. by hive-site) must NOT set self.MonacoEnvironment — that host
// installs its own (CDN-loaded) workers, and having the bundled `?worker`
// version overwrite it breaks worker startup. `?worker` imports add only a tiny
// wrapper here, not Monaco core, so this stays out of the lazy-loading budget.
import "./monacoWorkers.ts";
import React from "react";
import ReactDOM from "react-dom/client";
import { HashRouter } from "react-router-dom";
import App from "./App.tsx";
import "./index.css";

// Apply saved theme before first render to avoid flash
const savedTheme = localStorage.getItem("app:theme") ?? "system";
if (
  savedTheme === "dark" ||
  (savedTheme === "system" &&
    window.matchMedia("(prefers-color-scheme: dark)").matches)
) {
  document.documentElement.classList.add("dark");
}

ReactDOM.createRoot(document.getElementById("root")!).render(
  <React.StrictMode>
    <HashRouter>
      <App />
    </HashRouter>
  </React.StrictMode>,
);
