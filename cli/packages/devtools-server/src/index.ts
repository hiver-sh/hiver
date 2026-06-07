#!/usr/bin/env node
import cors from "cors";
import express from "express";
import { existsSync, readFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import sandboxRoutes from "./routes/sandboxes.js";
import configRoutes from "./routes/config.js";
import fileRoutes from "./routes/files.js";
import portRoutes from "./routes/ports.js";
import eventsRoutes from "./routes/events.js";
import terminalRoutes from "./routes/terminal.js";
import traceRoutes from "./routes/trace.js";
import { DEFAULT_URL } from "./lib/gatewayUrl.js";

const app = express();
const PORT = process.env.PORT ? parseInt(process.env.PORT) : 3001;

app.use(cors({ allowedHeaders: ["Content-Type", "x-gateway-url"] }));
app.use(express.json());

app.use("/api/sandboxes", sandboxRoutes);
app.use("/api/sandboxes", configRoutes);
app.use("/api/sandboxes", fileRoutes);
app.use("/api/sandboxes", portRoutes);
app.use("/api/sandboxes", eventsRoutes);
app.use("/api/sandboxes", terminalRoutes);
app.use("/api/trace", traceRoutes);

const __dirname = dirname(fileURLToPath(import.meta.url));
const clientDist = join(__dirname, "../../devtools-client/dist");
if (existsSync(clientDist)) {
  app.use(express.static(clientDist, { index: false }));
  // Inject the configured gateway URL so the web client defaults to it instead
  // of its own hard-coded default.
  const indexHtml = readFileSync(
    join(clientDist, "index.html"),
    "utf8",
  ).replace(
    "</head>",
    `<script>window.__HIVE_GATEWAY_URL__=${JSON.stringify(DEFAULT_URL)}</script></head>`,
  );
  app.get("*", (_req, res) => res.type("html").send(indexHtml));
}

app.listen(PORT, () => {
  console.log(`DevTools server on http://localhost:${PORT}`);
  console.log(`Default gateway: ${DEFAULT_URL}`);
  if (existsSync(clientDist)) console.log(`Serving client from ${clientDist}`);
});
