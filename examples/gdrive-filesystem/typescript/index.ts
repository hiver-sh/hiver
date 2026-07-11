// Provision a sandbox running the `mcp-server` image with a Google
// Drive filesystem mounted at /workspace, then point the MCP
// Inspector at it so you can poke at the drive through the proxied
// MCP server.
//
// Credential flow:
//   1. Stdin: paste OAuth client_id + client_secret (Desktop-app
//      client from https://console.cloud.google.com/apis/credentials).
//   2. Browser: visit the printed auth URL, approve access. The
//      example runs a one-shot localhost listener to receive the
//      OAuth callback and exchanges the code for access + refresh
//      tokens via the googleapis SDK.
//   3. Drive top-level folders are listed; pick one by number.
//   4. The sandbox is created with the resolved gdrive_* params.
//
// Run with: npm install && npm start
import { randomBytes } from "node:crypto";
import { createServer } from "node:http";
import { AddressInfo } from "node:net";
import process from "node:process";
import { createInterface } from "node:readline/promises";
import { google } from "googleapis";

import * as hiver from "@hiver.sh/client";

const rl = createInterface({ input: process.stdin, output: process.stdout });

async function ask(label: string): Promise<string> {
  while (true) {
    const value = (await rl.question(`${label}: `)).trim();
    if (value) return value;
    console.warn(`  ${label} is required.`);
  }
}

console.info("Enter Google OAuth client credentials (Desktop app type):");
const clientId = await ask("client_id");
const clientSecret = await ask("client_secret");

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
  clientId,
  clientSecret,
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
rl.close();

const sandbox = await hiver.getOrCreateSandbox("hive-gdrive", {
  fs: [
    {
      backend: "gdrive",
      mount: "/workspace",
      acls: [{ path: "/workspace/**", access: "rw" }],
      gdrive_access_token: tokens.access_token,
      gdrive_refresh_token: tokens.refresh_token,
      gdrive_client_id: clientId,
      gdrive_client_secret: clientSecret,
      gdrive_folder_id: folderId,
    },
  ],
});

const ac = new AbortController();

for await (const event of sandbox.getEventsStream({ signal: ac.signal })) {
  console.info("sandbox event", event);
}
