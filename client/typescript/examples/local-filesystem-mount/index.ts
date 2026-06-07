// Mounts a local directory into the sandbox.
//
// Useful for local development.
//
// Run with: npx tsx examples/local-filesystem-mount
import * as path from "path";
import { fileURLToPath } from "url";
import * as hiver from "@hiver.sh/client";

const __dirname = path.dirname(fileURLToPath(import.meta.url));

const sandboxConfig: hiver.SandboxConfig = {
  ttl: 1800,
  fs: [
    {
      backend: "local",
      mount: "/workspace",
      origin: path.resolve(__dirname, "skills"),
      acls: [{ path: "/workspace/**", access: "ro" }],
    },
  ],
};

const sandbox = await hiver.getOrCreateSandbox("hive-example", sandboxConfig);

const readFile = async (file: string) => {
  const bytes = await sandbox.downloadFile(file);
  console.log("\n");
  console.log(`reading file: ${file}`);
  console.log(new TextDecoder().decode(bytes));
};

await readFile("/workspace/echo/SKILL.md");
await readFile("/workspace/echo/echo.sh");

await hiver.shutdown(sandbox);
