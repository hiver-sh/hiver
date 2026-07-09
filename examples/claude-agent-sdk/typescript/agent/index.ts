import express from "express";
import { query } from "@anthropic-ai/claude-agent-sdk";

const app = express();
app.use(express.json());

app.post("/chat", async (req, res) => {
  let reply = "";
  for await (const msg of query({
    prompt: req.body.prompt,
    options: {
      model: "claude-opus-4-8",
      cwd: "/workspace",
      allowedTools: [
        "Bash",
        "Read",
        "Write",
        "Edit",
        "Glob",
        "Grep",
        "WebSearch",
      ],
      permissionMode: "bypassPermissions",
    },
  })) {
    if (msg.type === "result" && msg.subtype === "success") {
      reply = msg.result;
    }
  }

  res.json({ reply });
});

app.listen(3000, () => console.log("listening on :3000"));
