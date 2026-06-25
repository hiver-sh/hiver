// Run an interactive Python REPL inside the sandbox with a TTY, writing to
// stdin to drive it programmatically.
//
// Run with: npx tsx examples/python-exec-tty.ts
import * as hiver from "@hiver.sh/client";

const sandbox = await hiver.getOrCreateSandbox("hiver-python-exec-tty", {
  image: "python",
  ttl: 0,
});

const exec = await sandbox.execStream("python3", {
  cwd: "/workspace",
  tty: true,
});

// Feed a short script to the REPL line by line, then ask it to exit.
const lines = ["x = 6 * 7", "print('the answer is', x)", "exit()"];

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
