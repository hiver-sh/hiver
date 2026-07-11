// Reads the current configuration, mutates it (adds an egress allow
// rule for api.github.com), and applies the update. The server diffs
// the supplied config against the running state and returns the
// concrete additions/removals in `result.changes`.
//
// Run with: npm install && npm start
import * as hiver from "@hiver.sh/client";

const sandbox = await hiver.getOrCreateSandbox("hiver-apply-config-example");

const current = await sandbox.getConfig();
console.info("current:", current);

const desired: hiver.SandboxConfig = {
  ...current,
  egress: [
    ...(current.egress ?? []),
    {
      access: "allow",
      host: "api.github.com",
      methods: ["GET"],
      paths: ["/repos/*"],
    },
  ],
};

const result = await sandbox.applyConfig(desired);

if (!result.applied) {
  console.error("apply rolled back:", result.error);
  process.exitCode = 1;
} else {
  console.info("changes:", JSON.stringify(result.changes, null, 2));
}
