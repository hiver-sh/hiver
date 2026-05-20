#!/usr/bin/env node
/**
 * Worker agent: stateful MCP server over HTTP.
 *
 * Session lifecycle:
 *   POST / (no Mcp-Session-Id)  → initialize session, returns session ID
 *   POST / (with session ID)    → send message to existing session
 *   GET  / (with session ID)    → SSE stream (required for elicitation)
 *   DELETE / (with session ID)  → close session
 *
 * Usage:
 *   ANTHROPIC_API_KEY=... node worker.js        # default port 3000
 *   PORT=8080 ANTHROPIC_API_KEY=... node worker.js
 */
import http, { IncomingMessage, ServerResponse } from "node:http";
import { randomUUID } from "node:crypto";
import { exec } from "node:child_process";
import { readFile, writeFile, mkdir } from "node:fs/promises";
import { dirname } from "node:path";
import { promisify } from "node:util";
import Anthropic from "@anthropic-ai/sdk";
import { McpServer } from "@modelcontextprotocol/sdk/server/mcp.js";
import { StreamableHTTPServerTransport } from "@modelcontextprotocol/sdk/server/streamableHttp.js";
import { z } from "zod";

const execAsync = promisify(exec);

const PORT = Number(process.env.PORT ?? 3000);
const MODEL = "claude-sonnet-4-6";

type Session = { server: McpServer; transport: StreamableHTTPServerTransport };
const sessions = new Map<string, Session>();

const CLAUDE_TOOLS: Anthropic.Messages.ToolUnion[] = [
  { type: "web_search_20260209", name: "web_search" },
  { type: "web_fetch_20260209", name: "web_fetch" },
  { type: "bash_20250124", name: "bash" },
  { type: "text_editor_20250728", name: "str_replace_based_edit_tool" },
  {
    name: "ask_orchestrator",
    description:
      "Ask the orchestrator for information or clarification you cannot obtain yourself. " +
      "Use when you are missing context, credentials, or decisions that only the caller can provide.",
    input_schema: {
      type: "object" as const,
      properties: {
        question: { type: "string", description: "The question to ask." },
      },
      required: ["question"],
    },
  },
];

const claude = new Anthropic();

type TextEditorInput =
  | { command: "view"; path: string; view_range?: [number, number] }
  | { command: "str_replace"; path: string; old_str: string; new_str: string }
  | { command: "create"; path: string; file_text: string }
  | { command: "insert"; path: string; insert_line: number; new_str: string };

async function runBash(command: string): Promise<string> {
  try {
    const { stdout, stderr } = await execAsync(command, { timeout: 120_000 });
    return stdout + (stderr ? `\n[stderr]\n${stderr}` : "");
  } catch (e: unknown) {
    const err = e as { stdout?: string; stderr?: string; message: string };
    return (
      (err.stdout ?? "") +
      (err.stderr ? `\n[stderr]\n${err.stderr}` : "") +
      `\n[error]\n${err.message}`
    );
  }
}

async function runTextEditor(input: TextEditorInput): Promise<string> {
  switch (input.command) {
    case "view": {
      const text = await readFile(input.path, "utf-8");
      if (!input.view_range) return text;
      const [start, end] = input.view_range;
      return text
        .split("\n")
        .slice(start - 1, end)
        .join("\n");
    }
    case "str_replace": {
      const text = await readFile(input.path, "utf-8");
      if (!text.includes(input.old_str))
        throw new Error(`str not found in ${input.path}`);
      await writeFile(
        input.path,
        text.replace(input.old_str, input.new_str),
        "utf-8",
      );
      return "OK";
    }
    case "create": {
      await mkdir(dirname(input.path), { recursive: true });
      await writeFile(input.path, input.file_text, "utf-8");
      return "OK";
    }
    case "insert": {
      const text = await readFile(input.path, "utf-8");
      const lines = text.split("\n");
      lines.splice(input.insert_line, 0, input.new_str);
      await writeFile(input.path, lines.join("\n"), "utf-8");
      return "OK";
    }
  }
}

async function agenticLoop(
  task: string,
  context: string,
  mcpServer: McpServer,
): Promise<string> {
  const prompt = context ? `Context:\n${context}\n\n${task}` : task;
  const messages: Anthropic.Messages.MessageParam[] = [
    { role: "user", content: prompt },
  ];

  while (true) {
    const stream = claude.messages.stream({
      model: MODEL,
      max_tokens: 8192,
      thinking: { type: "adaptive" },
      tools: CLAUDE_TOOLS,
      messages,
    });

    for await (const event of stream) {
      if (event.type === "content_block_delta") {
        console.log(event.delta);
      }
    }

    const response = await stream.finalMessage();

    messages.push({ role: "assistant", content: response.content });

    if (response.stop_reason === "end_turn") {
      const text = response.content.find(
        (b): b is Anthropic.Messages.TextBlock => b.type === "text",
      );
      return text?.text ?? "";
    }

    // server-side tool loop limit reached — just continue
    if (response.stop_reason === "pause_turn") {
      messages.push({ role: "user", content: [] });
      continue;
    }

    const toolResults: Anthropic.Messages.ToolResultBlockParam[] = [];

    for (const block of response.content) {
      if (block.type !== "tool_use") continue;

      if (block.name === "bash") {
        const { command } = block.input as { command: string };
        const content = await runBash(command);
        toolResults.push({
          type: "tool_result",
          tool_use_id: block.id,
          content,
        });
        continue;
      }

      if (block.name === "str_replace_based_edit_tool") {
        let content: string;
        try {
          content = await runTextEditor(block.input as TextEditorInput);
        } catch (e: unknown) {
          content = `Error: ${(e as Error).message}`;
        }
        toolResults.push({
          type: "tool_result",
          tool_use_id: block.id,
          content,
        });
        continue;
      }

      if (block.name === "ask_orchestrator") {
        const { question } = block.input as { question: string };

        const elicit = await mcpServer.server.elicitInput({
          message: question,
          requestedSchema: {
            type: "object",
            properties: {
              answer: {
                type: "string",
                description: "Your answer to the worker's question.",
              },
            },
            required: ["answer"],
          },
        });

        const answer =
          elicit.action === "accept"
            ? ((elicit.content?.["answer"] as string | undefined) ?? "")
            : `Orchestrator ${elicit.action}d the request.`;

        toolResults.push({
          type: "tool_result",
          tool_use_id: block.id,
          content: answer,
        });
      }
      // web_search / web_fetch are server-side — their results flow back
      // to Claude automatically; no tool_result needed from us.
    }

    if (toolResults.length > 0) {
      messages.push({ role: "user", content: toolResults });
    }
  }
}

function makeServer(): McpServer {
  const server = new McpServer({ name: "worker-agent", version: "0.1.0" });

  server.registerTool(
    "run_task",
    {
      description:
        "Run a task using a full Claude agentic loop. Claude can search the web, " +
        "fetch URLs, run bash commands, read and write files, and ask the orchestrator " +
        "for clarification via elicitation. Include all context the worker needs; " +
        "it has no memory across calls.",
      inputSchema: {
        task: z.string().describe("The task to complete."),
        context: z
          .string()
          .optional()
          .describe("Background context the worker needs."),
      },
    },
    async ({ task, context = "" }) => {
      const result = await agenticLoop(task, context, server);
      return { content: [{ type: "text" as const, text: result }] };
    },
  );

  return server;
}

async function readBody(req: IncomingMessage): Promise<unknown> {
  return new Promise((resolve, reject) => {
    let raw = "";
    req.on("data", (chunk: Buffer) => {
      raw += chunk;
    });
    req.on("end", () => {
      try {
        resolve(raw ? JSON.parse(raw) : undefined);
      } catch (e) {
        reject(e);
      }
    });
    req.on("error", reject);
  });
}

const CORS_HEADERS: Record<string, string> = {
  "Access-Control-Allow-Origin": "*",
  "Access-Control-Allow-Methods": "GET, POST, PUT, DELETE, OPTIONS",
  "Access-Control-Allow-Headers": "*",
  "Access-Control-Expose-Headers": "Mcp-Session-Id",
};

function setCors(res: ServerResponse) {
  for (const [k, v] of Object.entries(CORS_HEADERS)) res.setHeader(k, v);

  // Monkey-patch writeHead so CORS headers survive if the transport calls
  // writeHead with its own header map (which would otherwise take precedence).
  const orig = res.writeHead.bind(res);
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  (res as any).writeHead = function (statusCode: number, ...args: any[]) {
    const last = args[args.length - 1];
    if (last != null && typeof last === "object" && !Array.isArray(last)) {
      args[args.length - 1] = { ...last, ...CORS_HEADERS };
    } else {
      args.push(CORS_HEADERS);
    }
    return orig(statusCode, ...args);
  };
}

const httpServer = http.createServer(
  async (req: IncomingMessage, res: ServerResponse) => {
    if (req.method === "OPTIONS") {
      setCors(res);
      res.writeHead(204).end();
      return;
    }
    setCors(res);
    if (req.url !== "/") {
      res.writeHead(404).end();
      return;
    }

    const body = req.method === "POST" ? await readBody(req) : undefined;
    const sessionId = req.headers["mcp-session-id"] as string | undefined;
    const session = sessionId ? sessions.get(sessionId) : undefined;
    const isInit =
      body != null &&
      typeof body === "object" &&
      (body as Record<string, unknown>)["method"] === "initialize";

    if (session && !isInit) {
      await session.transport.handleRequest(req, res, body);
      return;
    }

    // Re-initialize: tear down stale session before creating a new one.
    // Clear onclose first to avoid close → onclose → close → … recursion.
    if (session) {
      session.transport.onclose = undefined;
      sessions.delete(sessionId!);
      session.server.close().catch(() => {});
    }

    if (req.method !== "POST") {
      res.writeHead(400).end();
      return;
    }

    const id = randomUUID();
    const server = makeServer();
    const transport = new StreamableHTTPServerTransport({
      sessionIdGenerator: () => id,
    });
    transport.onclose = () => {
      sessions.delete(id);
      server.close();
    };
    sessions.set(id, { server, transport });
    await server.connect(transport);
    await transport.handleRequest(req, res, body);
  },
);

httpServer.listen(PORT, () => {
  console.log(`worker-agent MCP server listening on http://0.0.0.0:${PORT}`);
});

process.on("SIGTERM", () => httpServer.close());
