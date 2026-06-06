// Hook the Claude Agent SDK up to a Hive-managed MCP server. The
// sandbox boots the `mcp-server` image, and `sandbox.exposedEndpoint` is handed to
// the SDK as an HTTP MCP server so every tool call the model makes is
// mediated by sandboxd's egress + FUSE policies.
//
// Run with:
//   ANTHROPIC_API_KEY=<value>
//   FINNHUB_API_KEY=<value> \
//   GOOGLE_CLIENT_ID=<value> \
//   GOOGLE_CLIENT_SECRET=<value> \
//     npx tsx examples/claude-agent.ts
//
// Grab a free Finnhub key at https://finnhub.io/register
// Grab a Google Client id and secret at https://console.cloud.google.com/apis/credential
import process from "node:process";
import { createInterface } from "node:readline/promises";
import { query, type SDKUserMessage } from "@anthropic-ai/claude-agent-sdk";
import { createShutdown } from "./shutdown.js";
import chalk from "chalk";

import { randomBytes } from "node:crypto";
import { createServer } from "node:http";
import { AddressInfo } from "node:net";
import { google } from "googleapis";

import * as hive from "../src";

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

const googleClientId = process.env.GOOGLE_CLIENT_ID;
if (!googleClientId) {
  console.error(
    "GOOGLE_CLIENT_ID must be defined (free key: https://console.cloud.google.com/apis/credentials)",
  );
  process.exit(1);
}

const googleClientSecret = process.env.GOOGLE_CLIENT_SECRET;
if (!googleClientSecret) {
  console.error(
    "GOOGLE_CLIENT_SECRET must be defined (get it on: https://console.cloud.google.com/apis/credentials)",
  );
  process.exit(1);
}

const rl = createInterface({ input: process.stdin, output: process.stdout });

// Bind a one-shot HTTP listener for the OAuth callback. Random port
// is fine — Desktop-app OAuth clients trust any 127.0.0.1:* redirect.
const callback = await new Promise<{
  port: number;
  next: Promise<{ code: string; state: string }>;
}>((resolve, reject) => {
  let resolveCode!: (v: { code: string; state: string }) => void;
  let rejectCode!: (e: Error) => void;
  const next = new Promise<{ code: string; state: string }>((res, rej) => {
    resolveCode = res;
    rejectCode = rej;
  });
  const server = createServer((req, res) => {
    const url = new URL(req.url ?? "/", "http://localhost");
    const code = url.searchParams.get("code");
    const state = url.searchParams.get("state");
    const err = url.searchParams.get("error");
    if (err) {
      res.writeHead(400).end(`OAuth error: ${err}`);
      rejectCode(new Error(`OAuth error: ${err}`));
    } else if (code && state) {
      res
        .writeHead(200, { "content-type": "text/plain" })
        .end("Authorization received. You can close this tab.");
      resolveCode({ code, state });
    } else {
      res.writeHead(400).end("missing code");
    }
    server.close();
  });
  server.on("error", reject);
  server.listen(0, "127.0.0.1", () => {
    const addr = server.address() as AddressInfo;
    resolve({ port: addr.port, next });
  });
});

const redirectUri = `http://127.0.0.1:${callback.port}/oauth/callback`;
const oauth2Client = new google.auth.OAuth2(
  googleClientId,
  googleClientSecret,
  redirectUri,
);
const state = randomBytes(16).toString("hex");
const authUrl = oauth2Client.generateAuthUrl({
  access_type: "offline",
  prompt: "consent",
  scope: ["https://www.googleapis.com/auth/drive"],
  state,
});

console.info("");
console.info("Open this URL in your browser to authorize Hive:");
console.info("  " + authUrl);
console.info("");
console.info(`Waiting for OAuth callback on ${redirectUri} ...`);

const { code, state: returnedState } = await callback.next;
if (returnedState !== state) {
  throw new Error("OAuth state mismatch — possible CSRF.");
}
const { tokens } = await oauth2Client.getToken(code);
if (!tokens.access_token || !tokens.refresh_token) {
  throw new Error(
    "OAuth response missing access_token or refresh_token — re-run and make sure the client is a Desktop app.",
  );
}
oauth2Client.setCredentials(tokens);
console.info("✓ Tokens obtained");

// List the user's top-level Drive folders so they can pick by number.
const drive = google.drive({ version: "v3", auth: oauth2Client });
const folders = await drive.files.list({
  q: "mimeType = 'application/vnd.google-apps.folder' and trashed = false and 'root' in parents",
  fields: "files(id, name)",
  pageSize: 100,
  orderBy: "name",
});
const items = (folders.data.files ?? []).filter(
  (f): f is { id: string; name: string } => !!f.id && !!f.name,
);
if (items.length === 0) {
  throw new Error("No top-level folders found in this Drive.");
}
console.info("");
console.info("Top-level Drive folders:");
items.forEach((f, i) => console.info(`  ${i + 1}. ${f.name}`));

const folderId = await (async () => {
  while (true) {
    const raw = (await rl.question("folder number: ")).trim();
    const n = Number.parseInt(raw, 10);
    if (Number.isFinite(n) && n >= 1 && n <= items.length) {
      return items[n - 1]!.id;
    }
    console.warn(`  enter a number between 1 and ${items.length}`);
  }
})();

const sandbox = await hive.getOrCreateSandbox("hive-claude-agent", {
  image: "mcp-server",
  fs: [
    {
      backend: "gdrive",
      mount: "/workspace",
      acls: [{ path: "/workspace/**", access: "rw" }],
      gdrive_access_token: tokens.access_token,
      gdrive_refresh_token: tokens.refresh_token,
      gdrive_client_id: googleClientId,
      gdrive_client_secret: googleClientSecret,
      gdrive_folder_id: folderId,
    },
  ],
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
    ...hive.allowedPythonPackages(
      "numpy",
      "pandas",
      "statsmodels",
      "scikit-learn",
      "matplotlib",
    ),
  ],
});

const { ac, shutdown } = createShutdown(sandbox, { cleanup: () => rl.close() });

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
      };
    }
  }
}

const response = query({
  prompt: prompts(),
  options: {
    model: "claude-opus-4-7",
    abortController: ac,
    includePartialMessages: true,
    tools: [],
    mcpServers: {
      sandbox: {
        type: "http",
        url: `http://${sandbox.exposedEndpoint!}`,
        alwaysLoad: true,
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

- '/workspace' is a PERSISTENT directory. All files written there survive across sessions.
- **Before doing any work, run 'ls -la /workspace/' and relevant subdirectories.** A previous session may have already fetched data, built models, or written reports that fully or partially answer the current question. Re-use them rather than repeating work.
- If an existing file already answers the question, read it and respond — skip fetching, computing, or regenerating.
- Save all new datasets, scripts, plots, and reports to '/workspace'. Organise by task — e.g. '/workspace/<ticker>/data.csv', '/workspace/<ticker>/model.py', '/workspace/<ticker>/report.md'.
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
  await shutdown();
}
