import { createServer } from "node:http";
import { execFile } from "node:child_process";
import { promisify } from "node:util";
import { readFile, writeFile, mkdir, readdir, stat } from "node:fs/promises";
import { join, dirname, relative, basename } from "node:path";
import { McpServer } from "@modelcontextprotocol/sdk/server/mcp.js";
import { StreamableHTTPServerTransport } from "@modelcontextprotocol/sdk/server/streamableHttp.js";
import { z } from "zod";

const execFileAsync = promisify(execFile);

const PORT = Number(process.env.PORT ?? 3000);
const DEFAULT_READ_LIMIT = 2000;


async function bash(cmd: string, cwd?: string): Promise<{ stdout: string; stderr: string; exitCode: number }> {
  try {
    const { stdout, stderr } = await execFileAsync("/bin/sh", ["-c", cmd], {
      cwd: cwd || undefined,
      maxBuffer: 10 * 1024 * 1024,
    });
    return { stdout, stderr, exitCode: 0 };
  } catch (err: any) {
    return {
      stdout: err.stdout ?? "",
      stderr: err.stderr ?? String(err),
      exitCode: err.code ?? 1,
    };
  }
}

async function readTool(
  path: string,
  offset = 0,
  limit = DEFAULT_READ_LIMIT,
): Promise<{ content: string; startLine: number; lineCount: number; truncated: boolean }> {
  const data = await readFile(path, "utf8");
  const lines = data.replace(/\n$/, "").split("\n");
  const slice = lines.slice(offset, offset + limit);
  const truncated = offset + slice.length < lines.length;
  return {
    content: slice.join("\n"),
    startLine: offset,
    lineCount: slice.length,
    truncated,
  };
}

async function writeTool(path: string, content: string): Promise<{ bytes: number }> {
  await mkdir(dirname(path), { recursive: true });
  await writeFile(path, content, "utf8");
  return { bytes: Buffer.byteLength(content, "utf8") };
}

async function editTool(
  path: string,
  oldString: string,
  newString: string,
  replaceAll = false,
): Promise<{ replacements: number }> {
  const src = await readFile(path, "utf8");
  const count = src.split(oldString).length - 1;
  if (count === 0) throw new Error(`oldString not found in ${path}`);
  if (!replaceAll && count > 1)
    throw new Error(`oldString matches ${count} times in ${path}; pass replaceAll=true or include more context`);

  const out = replaceAll ? src.split(oldString).join(newString) : src.replace(oldString, newString);
  await writeFile(path, out, "utf8");
  return { replacements: replaceAll ? count : 1 };
}

async function* walkDir(dir: string): AsyncGenerator<string> {
  const entries = await readdir(dir, { withFileTypes: true });
  for (const e of entries) {
    const full = join(dir, e.name);
    if (e.isDirectory()) yield* walkDir(full);
    else yield full;
  }
}

function matchGlob(pattern: string, name: string): boolean {
  if (!pattern.includes("**")) {
    return fnmatch(pattern, name);
  }
  const parts = pattern.split("**");
  const first = parts[0]!.replace(/\/$/, "");
  if (first) {
    const prefix = first + "/";
    if (!name.startsWith(prefix) && name !== first) return false;
    name = name.startsWith(prefix) ? name.slice(prefix.length) : name.slice(first.length);
  }
  for (let i = 1; i < parts.length; i++) {
    const seg = parts[i]!.replace(/^\//, "");
    if (!seg) return true;
    const last = i === parts.length - 1;
    if (last) {
      for (let j = 0; j <= name.length; j++) {
        if (fnmatch(seg, name.slice(j))) return true;
      }
      return false;
    }
    let found = -1;
    for (let j = 0; j <= name.length; j++) {
      const slash = name.indexOf("/", j);
      if (slash < 0) break;
      if (fnmatch(seg, name.slice(j, slash))) { found = slash + 1; break; }
    }
    if (found < 0) return false;
    name = name.slice(found);
  }
  return true;
}

function fnmatch(pattern: string, name: string): boolean {
  const re = "^" + pattern.replace(/[.+^${}()|[\]\\]/g, "\\$&").replace(/\*/g, "[^/]*").replace(/\?/g, "[^/]") + "$";
  return new RegExp(re).test(name);
}

async function globTool(pattern: string, root = "/"): Promise<{ paths: string[] }> {
  const paths: string[] = [];
  try {
    for await (const full of walkDir(root)) {
      const rel = relative(root, full);
      if (matchGlob(pattern, rel) || matchGlob(pattern, basename(full))) {
        paths.push(full);
      }
    }
  } catch {
    // dir may not exist
  }
  return { paths };
}

async function grepTool(pattern: string, path: string): Promise<{ matches: { path: string; line: number; text: string }[] }> {
  const re = new RegExp(pattern);
  const matches: { path: string; line: number; text: string }[] = [];

  async function grepFile(filePath: string) {
    let data: string;
    try {
      data = await readFile(filePath, "utf8");
    } catch {
      return;
    }
    // skip binary files
    const head = Buffer.from(data.slice(0, 512));
    if (head.includes(0)) return;
    data.split("\n").forEach((line, i) => {
      if (re.test(line)) matches.push({ path: filePath, line: i + 1, text: line });
    });
  }

  let info: Awaited<ReturnType<typeof stat>>;
  try { info = await stat(path); } catch (err) { throw err; }

  if (info.isDirectory()) {
    for await (const file of walkDir(path)) await grepFile(file);
  } else {
    await grepFile(path);
  }
  return { matches };
}

// ── MCP server setup ──────────────────────────────────────────────────────────

function buildMcpServer(): McpServer {
  const server = new McpServer({ name: "sandbox-mcp-server", version: "1.0.0" });

  server.registerTool("bash", {
    description:
      "Execute a shell command and return stdout, stderr, and exit code. " +
      "Use 'read'/'write'/'edit'/'glob'/'grep' before falling back to 'bash' equivalents — they are typed, faster, and produce cleaner diffs.",
    inputSchema: {
      cmd: z.string().describe("Shell command to execute via /bin/sh -c"),
      cwd: z.string().optional().describe("Absolute working directory. Defaults to the process cwd."),
    },
  }, async ({ cmd, cwd }) => ({
    content: [{ type: "text", text: JSON.stringify(await bash(cmd, cwd)) }],
  }));

  server.registerTool("read", {
    description: "Read the contents of a file. Use this instead of 'cat' when you only need to inspect a file.",
    inputSchema: {
      path: z.string().describe("Absolute path of the file to read"),
      offset: z.number().int().optional().describe("0-based line index to start reading from. Defaults to 0"),
      limit: z.number().int().optional().describe("Maximum number of lines to return. Defaults to 2000"),
    },
  }, async ({ path, offset, limit }) => ({
    content: [{ type: "text", text: JSON.stringify(await readTool(path, offset, limit)) }],
  }));

  server.registerTool("write", {
    description:
      "Write contents to a file, creating parent directories as needed. " +
      "Use this instead of shell redirection so the file is captured atomically.",
    inputSchema: {
      path: z.string().describe("Absolute path of the file to write"),
      content: z.string().describe("File contents to write"),
    },
  }, async ({ path, content }) => ({
    content: [{ type: "text", text: JSON.stringify(await writeTool(path, content)) }],
  }));

  server.registerTool("edit", {
    description:
      "Replace a substring in a file. " +
      "Cheaper than rewriting the whole file when you're tweaking a script or report.",
    inputSchema: {
      path: z.string().describe("Absolute path of the file to edit"),
      oldString: z.string().describe("Substring to replace"),
      newString: z.string().describe("Replacement string"),
      replaceAll: z.boolean().optional().describe("Replace every occurrence; otherwise oldString must match exactly once"),
    },
  }, async ({ path, oldString, newString, replaceAll }) => ({
    content: [{ type: "text", text: JSON.stringify(await editTool(path, oldString, newString, replaceAll)) }],
  }));

  server.registerTool("glob", {
    description: "Find files matching a glob pattern. (e.g. '**/*.csv').",
    inputSchema: {
      pattern: z.string().describe("Glob pattern. Supports *, ?, [class] and ** for any number of path segments"),
      root: z.string().optional().describe("Directory to search under. Defaults to /"),
    },
  }, async ({ pattern, root }) => ({
    content: [{ type: "text", text: JSON.stringify(await globTool(pattern, root)) }],
  }));

  server.registerTool("grep", {
    description: "Search files for lines matching a regular expression.",
    inputSchema: {
      pattern: z.string().describe("Regular expression to search for"),
      path: z.string().describe("File or directory to search. Directories are searched recursively"),
    },
  }, async ({ pattern, path }) => ({
    content: [{ type: "text", text: JSON.stringify(await grepTool(pattern, path)) }],
  }));

  return server;
}

// ── HTTP server ───────────────────────────────────────────────────────────────

const httpServer = createServer(async (req, res) => {
  if (req.url !== "/mcp" && req.url !== "/mcp/") {
    res.writeHead(404).end();
    return;
  }
  const transport = new StreamableHTTPServerTransport({ sessionIdGenerator: undefined });
  const mcpServer = buildMcpServer();
  await mcpServer.connect(transport);
  await transport.handleRequest(req, res);
});

httpServer.listen(PORT, () => {
  console.log(`mcp-server listening on :${PORT}/mcp`);
});
