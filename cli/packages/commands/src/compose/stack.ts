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

// Sandbox containers are spawned by the controller outside the compose file
// (they share the `com.docker.compose.project` label but aren't declared
// services), so `docker compose down` leaves them running. This is the
// compose project / namespace they're tagged with.
const NAMESPACE = "hiver";

/**
 * Force-remove every container in the `hiver` namespace that compose down won't
 * reap — i.e. the controller-spawned sandbox containers. Returns the count
 * removed (0 if none or docker errors).
 */
export function removeNamespaceContainers(): number {
  const ps = spawnSync(
    "docker",
    ["ps", "-aq", "--filter", `label=com.docker.compose.project=${NAMESPACE}`],
    { encoding: "utf8" },
  );
  if (ps.status !== 0) return 0;
  const ids = ps.stdout
    .split("\n")
    .map((s) => s.trim())
    .filter(Boolean);
  if (ids.length === 0) return 0;
  const rm = spawnSync("docker", ["rm", "-f", ...ids], { encoding: "utf8" });
  return rm.status === 0 ? ids.length : 0;
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
