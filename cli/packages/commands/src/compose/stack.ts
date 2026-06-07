import { spawnSync } from "node:child_process";

/** Container IDs of the stack's currently-running services (empty ⇒ stopped). */
export function runningContainers(composeFile: string): string[] {
  const res = spawnSync("docker", ["compose", "-f", composeFile, "ps", "-q"], {
    encoding: "utf8",
  });
  if (res.status !== 0) return [];
  return res.stdout
    .split("\n")
    .map((s) => s.trim())
    .filter(Boolean);
}

/** The host port a running service publishes for `containerPort`, if any. */
export function publishedPort(
  composeFile: string,
  service: string,
  containerPort: number,
): number | undefined {
  const res = spawnSync(
    "docker",
    ["compose", "-f", composeFile, "port", service, String(containerPort)],
    { encoding: "utf8" },
  );
  if (res.status !== 0) return undefined;
  const match = res.stdout.trim().match(/:(\d+)\s*$/);
  return match ? Number(match[1]) : undefined;
}
