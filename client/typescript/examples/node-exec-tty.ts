// Run an interactive Node.js REPL inside the sandbox with a TTY, writing to
// stdin to drive it programmatically.
//
// Run with: npx tsx examples/node-exec-tty.ts
import * as hiver from "@hiver.sh/client";

const sandbox = await hiver.getOrCreateSandbox("hiver-node-exec-tty", {
  image: "hiversh/node:alpine",
});

const exec = await sandbox.execStream("node", { cwd: "/workspace", tty: true });

const lines = ["const x = 6 * 7;", "console.log('the answer is', x);", ".exit"];

(async () => {
  for (const line of lines) {
    await exec.writeStdin(line + "\r");
  }
})();

for await (const pipe of exec.pipes) {
  if (pipe.stdout) process.stdout.write(pipe.stdout);
  if (pipe.stderr) process.stderr.write(pipe.stderr);
}

console.info("\nexit code:", await exec.exitCode);
