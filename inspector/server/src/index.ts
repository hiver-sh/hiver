import cors from "cors";
import express from "express";
import sandboxRoutes from "./routes/sandboxes.js";
import configRoutes from "./routes/config.js";
import fileRoutes from "./routes/files.js";
import eventsRoutes from "./routes/events.js";
import terminalRoutes from "./routes/terminal.js";
import { DEFAULT_URL } from "./lib/controllerUrl.js";

const app = express();
const PORT = process.env.PORT ? parseInt(process.env.PORT) : 3001;

app.use(cors());
app.use(express.json());

app.use("/api/sandboxes", sandboxRoutes);
app.use("/api/sandboxes", configRoutes);
app.use("/api/sandboxes", fileRoutes);
app.use("/api/sandboxes", eventsRoutes);
app.use("/api/sandboxes", terminalRoutes);

app.listen(PORT, () => {
  console.log(`Inspector server on http://localhost:${PORT}`);
  console.log(`Default controller: ${DEFAULT_URL}`);
});
