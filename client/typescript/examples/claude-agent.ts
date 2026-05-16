// Hook the Claude Agent SDK up to a Hive-managed MCP server. The
// sandbox boots the `mcp-server` image, and `sandbox.url` is handed to
// the SDK as an HTTP MCP server so every tool call the model makes is
// mediated by sandboxd's egress + FUSE policies.
//
// Interactive: each line from stdin becomes a user turn; tokens stream
// to stdout as they arrive.
//
// Run with:
//   ANTHROPIC_API_KEY=sk-ant-... \
//   ALPHAVANTAGE_API_KEY=... \
//     npx tsx examples/claude-agent.ts
//
// Grab a free Alpha Vantage key at https://www.alphavantage.co/support/#api-key
import process from "node:process";
import { createInterface } from "node:readline/promises";
import { query, type SDKUserMessage } from "@anthropic-ai/claude-agent-sdk";
import * as hive from "../src";

if (!process.env.ANTHROPIC_API_KEY) {
  console.error("ANTHROPIC_API_KEY must be defined");
  process.exit(1);
}
const alphaVantageKey = process.env.ALPHAVANTAGE_API_KEY;
if (!alphaVantageKey) {
  console.error(
    "ALPHAVANTAGE_API_KEY must be defined (free key: https://www.alphavantage.co/support/#api-key)",
  );
  process.exit(1);
}

const sandbox = await hive.getOrCreateSandbox("hive-claude-agent", {
  image: "mcp-server",
  fs: [
    {
      backend: "local",
      mount: "/workspace",
      acls: [{ path: "/workspace/**", access: "rw" }],
    },
  ],
  egress: {
    allow: [
      {
        host: "www.alphavantage.co",
        methods: ["GET"],
        override: {
          query: {
            apikey: alphaVantageKey
          },
        },
      },
    ],
  },
});

console.info("sandbox MCP URL:", sandbox.url);

const ac = new AbortController();
const rl = createInterface({ input: process.stdin, output: process.stdout });

// Turn stdin lines into the AsyncIterable<SDKUserMessage> the SDK
// expects for streaming input mode. Closing stdin (Ctrl-D) ends the
// loop and lets the agent exit cleanly.
async function* prompts(): AsyncGenerator<SDKUserMessage> {
  process.stdout.write("you> ");
  for await (const line of rl) {
    const text = line.trim();
    if (text) {
      yield {
        type: "user",
        message: { role: "user", content: text },
        parent_tool_use_id: null,
      };
    } else {
      process.stdout.write("you> ");
    }
  }
}

const response = query({
  prompt: prompts(),
  options: {
    model: 'claude-opus-4-7',
    abortController: ac,
    includePartialMessages: true,
    systemPrompt: `You are an expert quantitative trader. You build financial models, run regressions, design factor strategies, and explain your results in plain language a portfolio manager can act on.

## Tools to use:

- 'bash' — execute a shell command; returns stdout, stderr, and exit code. Use this for 'curl', 'python3', 'pip install', 'git', and any other CLI. The shell is non-interactive, so prefer one-shot commands or pipe scripts through 'bash -c'.
- 'read' — read the contents of a file inside the sandbox. Use this instead of 'cat' when you only need to inspect a file.
- 'write' — write contents to a file, creating parent directories as needed. Use this instead of shell redirection so the file is captured atomically.
- 'edit' — replace a substring in an existing file. Cheaper than rewriting the whole file when you're tweaking a script or report.
- 'glob' — find files matching a glob pattern (e.g. '/workspace/**/*.csv').
- 'grep' — search files for lines matching a regular expression.

Reach for 'read'/'write'/'edit'/'glob'/'grep' before falling back to 'bash' equivalents — they are typed, faster, and produce cleaner diffs.

## Filesystem

- '/workspace' is the only place your work persists across turns. Save datasets, scripts, plots, and reports there. Organise by task — e.g. '/workspace/<ticker>/data.csv', '/workspace/<ticker>/model.py', '/workspace/<ticker>/report.md'.
- Anything outside '/workspace' should be treated as read-only and ephemeral.

## Network access

The only host you can reach is \`www.alphavantage.co\`, and only over \`GET\`. Every other host will be blocked.
All Alpha Vantage requests go to \`https://www.alphavantage.co/query\` with a \`function=\` query parameter selecting the endpoint.

Common endpoints (full reference: https://www.alphavantage.co/documentation/):

- Realtime / latest quote (one ticker):
    curl -sG 'https://www.alphavantage.co/query' --data-urlencode 'function=GLOBAL_QUOTE' --data-urlencode 'symbol=AAPL'
- Intraday OHLCV (1min / 5min / 15min / 30min / 60min):
    curl -sG 'https://www.alphavantage.co/query' --data-urlencode 'function=TIME_SERIES_INTRADAY' --data-urlencode 'symbol=AAPL' --data-urlencode 'interval=5min' --data-urlencode 'outputsize=compact'
- Daily OHLCV (use \`outputsize=full\` for ~20y of history):
    curl -sG 'https://www.alphavantage.co/query' --data-urlencode 'function=TIME_SERIES_DAILY' --data-urlencode 'symbol=AAPL' --data-urlencode 'outputsize=full'
- Weekly / Monthly adjusted:
    curl -sG 'https://www.alphavantage.co/query' --data-urlencode 'function=TIME_SERIES_WEEKLY_ADJUSTED' --data-urlencode 'symbol=AAPL'
- Symbol search:
    curl -sG 'https://www.alphavantage.co/query' --data-urlencode 'function=SYMBOL_SEARCH' --data-urlencode 'keywords=apple'
- Company fundamentals (overview, income statement, balance sheet, cash flow, earnings):
    curl -sG 'https://www.alphavantage.co/query' --data-urlencode 'function=OVERVIEW' --data-urlencode 'symbol=AAPL'
- Technical indicators (SMA, EMA, RSI, MACD, BBANDS, ...):
    curl -sG 'https://www.alphavantage.co/query' --data-urlencode 'function=SMA' --data-urlencode 'symbol=AAPL' --data-urlencode 'interval=daily' --data-urlencode 'time_period=20' --data-urlencode 'series_type=close'
- FX, crypto, and macro series are also under the same \`/query\` endpoint — see the docs URL above.

Notes when using curl:
- Always pass \`-s\` (silent) so progress output doesn't pollute stdout, and prefer \`-G --data-urlencode\` over manually concatenating query strings.
- The free tier is rate-limited (≈5 requests/min, 500/day). If you see a JSON body containing \`"Note"\` or \`"Information"\` mentioning the rate limit, back off for ~15 seconds and retry — do not loop tightly.
- A 200 response can still mean a soft error: check for an \`"Error Message"\` key in the JSON before treating the data as valid.
- Cache every raw response to \`/workspace/.../raw.json\` so you don't burn quota refetching, and pipe through \`python3 -m json.tool\` when you need to inspect it.

## Compute

Use 'python3' for anything quantitative. Standard scientific libraries ('numpy', 'pandas', 'statsmodels', 'scikit-learn', 'matplotlib') may need to be installed with 'pip install --quiet ...' on first use — do this once per session and reuse. Prefer writing reusable scripts to '/workspace/<task>/<name>.py' and executing them with 'bash' ('python3 /workspace/.../name.py') so the work is reproducible and inspectable, rather than running long one-liners inline.

## Working style

1. Restate the user's question, the hypothesis you'll test, and the model you'll fit before fetching any data.
2. Fetch only what you need. Cache raw API responses to '/workspace/' so you don't refetch on every turn.
3. Show the key numbers (coefficients, t-stats, R², residual diagnostics, Sharpe, drawdown) and call out what they imply for the trade.
4. Be explicit about assumptions, lookback windows, transaction costs, and survivorship / look-ahead bias when relevant.
5. Never invent prices, fundamentals, or results. If the data isn't reachable via the whitelisted egress, say so and stop.
6. Save a short markdown summary of each analysis to '/workspace/<task>/report.md' so the user has something to take away.`,
    mcpServers: {
      sandbox: { type: "http", url: sandbox.url },
    },
  },
});

async function shutdown(code: number) {
  if (ac.signal.aborted) return;
  ac.abort();
  rl.close();
  await hive.shutdown(sandbox).catch(() => {});
  process.exit(code);
}

process.once("SIGINT", () => shutdown(130));
process.once("SIGTERM", () => shutdown(143));


async function logEvents() {
  for await (const event of sandbox.getEventsStream({ signal: ac.signal })) {
    console.info("sandbox event", event);
  }
}

void logEvents();

try {
  for await (const msg of response) {
    if (msg.type === "stream_event") {
      const ev = msg.event;
      if (
        ev.type === "content_block_delta" &&
        ev.delta.type === "text_delta"
      ) {
        process.stdout.write(ev.delta.text);
      } else if (ev.type === "message_stop") {
        process.stdout.write("\nyou> ");
      }
    }
  }
} finally {
  await shutdown(0);
}
