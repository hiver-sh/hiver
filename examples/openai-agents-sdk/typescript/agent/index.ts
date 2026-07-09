import express from "express";
import { promisify } from "node:util";
import { execFile } from "node:child_process";
import { readFile, writeFile } from "node:fs/promises";
import { Agent, run, tool } from "@openai/agents";
import { z } from "zod";

const sh = promisify(execFile);

// The SDK ships no local file tools, so define them as function tools. The
// agent runs inside the sandbox, so they resolve against its filesystem.
const bash = tool({
  name: "bash",
  description: "Run a shell command in /workspace and return its output.",
  parameters: z.object({ command: z.string() }),
  execute: async ({ command }) => {
    const { stdout, stderr } = await sh("bash", ["-lc", command], {
      cwd: "/workspace",
    });
    return stdout || stderr;
  },
});

const readFileTool = tool({
  name: "read_file",
  description: "Read a file from /workspace.",
  parameters: z.object({ path: z.string() }),
  execute: ({ path }) => readFile(`/workspace/${path}`, "utf8"),
});

const writeFileTool = tool({
  name: "write_file",
  description: "Write content to a file in /workspace.",
  parameters: z.object({ path: z.string(), content: z.string() }),
  execute: async ({ path, content }) => {
    await writeFile(`/workspace/${path}`, content);
    return `wrote ${path}`;
  },
});

const agent = new Agent({
  name: "coder",
  model: "gpt-5",
  instructions: "You are a coding agent working in /workspace.",
  tools: [bash, readFileTool, writeFileTool],
});

const app = express();
app.use(express.json());

app.post("/chat", async (req, res) => {
  const result = await run(agent, req.body.prompt);
  res.json({ reply: result.finalOutput });
});

app.listen(3000, () => console.log("listening on :3000"));
