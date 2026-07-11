// Hook the Claude Agent SDK up to a Hiver-managed MCP server. The
// `mcp-server/image` is built with the hiver CLI, the sandbox boots that
// bundle, and `sandbox.proxyUrl(3000)` is handed to the SDK as an HTTP MCP
// server so every tool call the model makes is mediated by the sandbox's
// egress + FUSE policies.
//
// Run with (build the image once, then start):
//   npm install && npm run build
//   ANTHROPIC_API_KEY=sk-ant-... FINNHUB_API_KEY=... npm start
//
// Grab a free Finnhub key at https://finnhub.io/register
import process from "node:process";
import { createInterface } from "node:readline/promises";
import { query, type SDKUserMessage } from "@anthropic-ai/claude-agent-sdk";
import chalk from "chalk";

import * as hiver from "@hiver.sh/client";

if (!process.env.ANTHROPIC_API_KEY) {
  console.error("ANTHROPIC_API_KEY must be defined");
  process.exit(1);
}
const finnhubKey = process.env.FINNHUB_API_KEY;
if (!finnhubKey) {
  console.error(
    "FINNHUB_API_KEY must be defined (free key: https://finnhub.io/register)",
  );
  process.exit(1);
}

// Build the image first with `npm run build` (hiver bundle ./image).
const imageTag = "mcp-server-image-bundle";

const sandbox = await hiver.getOrCreateSandbox("hive-claude-agent", {
  image: imageTag,
  egress: [
    {
      access: "allow",
      host: "finnhub.io",
      paths: ["/api/v1/*", "/static/swagger.json"],
      override: {
        headers: {
          "X-Finnhub-Token": finnhubKey,
        },
      },
    },
    ...hiver.allowedPythonPackages(
      "numpy",
      "pandas",
      "statsmodels",
      "scikit-learn",
      "matplotlib",
    ),
  ],
});

const rl = createInterface({ input: process.stdin, output: process.stdout });
const ac = new AbortController();

const YOU = chalk.green("you>");
let currentMsgHasToolUse = false;
let atLineStart = true;
let currentToolName = "";
let currentInputJson = "";

process.stdout.write(
  "\n" +
    chalk.bold("Expert Quantitative Trader\n") +
    "Build financial models, run regressions, design factor strategies, and explain your results in plain language a " +
    "portfolio manager can act on.\n\n" +
    chalk.gray("Example Prompts\n") +
    chalk.gray(
      "* Compare the performance of google and nvidia over the last 12 months",
    ) +
    "\n\n",
);

rl.setPrompt(YOU + " ");
rl.prompt();

async function* prompts(): AsyncGenerator<SDKUserMessage> {
  for await (const line of rl) {
    const text = line.trim();
    if (text) {
      process.stdout.write("\n");
      yield {
        type: "user",
        message: { role: "user", content: text },
        parent_tool_use_id: null,
        session_id: "",
      };
    }
  }
}

const response = query({
  prompt: prompts(),
  options: {
    model: "claude-opus-4-8",
    abortController: ac,
    includePartialMessages: true,
    tools: [],
    mcpServers: {
      sandbox: {
        type: "http",
        url: `${sandbox.proxyUrl(3000)}mcp`,
      },
    },
    allowedTools: [
      "mcp__sandbox__bash",
      "mcp__sandbox__read",
      "mcp__sandbox__write",
      "mcp__sandbox__edit",
      "mcp__sandbox__glob",
      "mcp__sandbox__grep",
    ],
    systemPrompt: `You are an expert quantitative trader.
You build financial models, run regressions, design factor strategies, and explain your results in plain language a portfolio manager can act on.

## Filesystem

- '/workspace' is an EXISTING directory, place your work to persist it across turns. Save datasets, scripts, plots, and reports there. Organise by task — e.g. '/workspace/<ticker>/data.csv', '/workspace/<ticker>/model.py', '/workspace/<ticker>/report.md'.
- Anything outside '/workspace' should be treated as read-only and ephemeral.

## API

The API documentation is saved in '/workspace/swagger.json'. ONLY if it doesn't exist, download https://finnhub.io/static/swagger.json and save as '/workspace/swagger.json'.

Use the swagger API documentation to discover endpoints and full reference.

Avoid endpoints that require premium access. Check for the "premium" field.

Notes when using curl:
- Always pass '-s' (silent) and prefer '-G --data-urlencode' over manually concatenating query strings.
- The free tier allows 60 API calls/minute. If you receive HTTP 429, sleep 10 seconds and retry — do not loop tightly.
- A 200 response with an empty '"s": "no_data"' field on candles means no data for that range; check before processing.
- Cache every raw response to '/workspace/.../raw.json' so you don't refetch on every turn, and pipe through 'python3 -m json.tool' when you need to inspect it.
- Candle 'from'/'to' timestamps: use 'int(datetime(year, month, day).timestamp())' in Python to convert.
- Auth token is already provided, so no need to send the token in the request.

## Compute

- USE 'jq' for processing JSON files. e.g.'/workspace/swagger.json'.
- USE 'python3' for anything quantitative. Standard scientific libraries ('numpy', 'pandas', 'statsmodels', 'scikit-learn', 'matplotlib')
may need to be installed with 'pip install --quiet ...' on first use — do this once per session and reuse.
Prefer writing reusable scripts to '/workspace/<task>/<name>.py' and executing them with 'bash' ('python3 /workspace/.../name.py')
so the work is reproducible and inspectable, rather than running long one-liners inline.

## Working style

- Restate the user's question, the hypothesis you'll test, and the model you'll fit before fetching any data.
- Fetch only what you need. Cache raw API responses to '/workspace/' so you don't refetch on every turn.
- Show the key numbers (coefficients, t-stats, R², residual diagnostics, Sharpe, drawdown) and call out what they imply for the trade.
- Be explicit about assumptions, lookback windows, transaction costs, and survivorship / look-ahead bias when relevant.
- Never invent prices, fundamentals, or results. If the data isn't reachable via the whitelisted egress, say so and stop.
- Save a short markdown summary of each analysis to '/workspace/<task>/report.md' so the user has something to take away.

## Response
- DON'T use markdown to respond. You are in a terminal, so your responses must be correct in the terminal.
- DON'T use ANSI escape codes to style the response.
- DON'T mention which endpoint you used or which endpoint is free or premium.
- Use emojis where possible.
`,
  },
});

try {
  for await (const msg of response) {
    if (msg.type === "stream_event") {
      const ev = msg.event;
      if (ev.type === "message_start") {
        // New assistant message — reset tool-use flag for this turn.
        currentMsgHasToolUse = false;
      } else if (
        ev.type === "content_block_start" &&
        ev.content_block.type === "tool_use"
      ) {
        // Model is about to call a tool — record its name and start collecting the input JSON.
        currentMsgHasToolUse = true;
        currentToolName = ev.content_block.name;
        currentInputJson = "";
      } else if (
        ev.type === "content_block_delta" &&
        ev.delta.type === "input_json_delta"
      ) {
        // Stream the tool's input JSON incrementally.
        currentInputJson += (ev.delta as any).partial_json ?? "";
      } else if (ev.type === "content_block_stop" && currentToolName) {
        // Tool input is complete — print the call in gray before it executes.
        let label = currentToolName;
        try {
          const params = JSON.stringify(JSON.parse(currentInputJson));
          const suffix =
            params.length > 100 ? params.slice(0, 97) + "…" : params;
          label += ` ${suffix}`;
        } catch {}
        if (!atLineStart) process.stdout.write("\n");
        process.stdout.write(chalk.gray(`→ ${label}\n\n`));
        atLineStart = true;
        currentToolName = "";
      } else if (
        ev.type === "content_block_delta" &&
        ev.delta.type === "text_delta"
      ) {
        // Stream text tokens directly to stdout as they arrive.
        process.stdout.write(ev.delta.text);
        atLineStart = ev.delta.text.endsWith("\n");
      } else if (ev.type === "message_stop" && !currentMsgHasToolUse) {
        // Final text message done (not a tool-use turn) — show the prompt for the next input.
        if (!atLineStart) process.stdout.write("\n");
        rl.prompt();
        atLineStart = false;
      }
    }
  }
} finally {
  rl.close();
}
