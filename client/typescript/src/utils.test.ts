import { expect, it } from "vitest";
import { allowSandbox } from "./utils";
import type { EgressRule, SandboxConfig } from "./schemas";

function pinnedBody(rules: EgressRule[]): SandboxConfig {
  const create = rules.find((r) => r.override?.body_strategy === "replace");
  expect(create).toBeDefined();
  return create!.override!.body as SandboxConfig;
}

// The pinned body replaces the nested create's request body verbatim, skipping
// getOrCreateSandbox's client-side defaulting. Without the same defaults a
// config with no `fs` creates a sandbox with NO workspace — while a resumed VM
// snapshot still holds its 9p /workspace mount, which is then orphaned and
// wedges the first process to touch it in p9_client_rpc.
it("allowSandbox pins the default /workspace fs when config has none", () => {
  const body = pinnedBody(
    allowSandbox("worker-1", {
      image: "browser",
      snapshot: { vm: { key: "browser" } },
    }),
  );
  expect(body.fs).toEqual([
    {
      backend: "local",
      mount: "/workspace",
      acls: [{ path: "/workspace/**", access: "rw" }],
    },
  ]);
  expect(body.egress).toEqual([{ host: "*", access: "allow" }]);
  expect(body.image).toBe("browser");
  expect(body.snapshot).toEqual({ vm: { key: "browser" } });
});

it("allowSandbox keeps an explicit fs and egress unchanged", () => {
  const fs = [{ backend: "local" as const, mount: "/data" }];
  const egress = [{ host: "example.com", access: "allow" as const }];
  const body = pinnedBody(allowSandbox("worker-1", { fs, egress }));
  expect(body.fs).toEqual(fs);
  expect(body.egress).toEqual(egress);
});

it("allowSandbox pins the same body on every create rule", () => {
  const rules = allowSandbox("worker-1", { image: "browser" });
  const bodies = rules
    .filter((r) => r.override?.body_strategy === "replace")
    .map((r) => r.override!.body);
  expect(bodies).toHaveLength(2); // docker + k8s gateway hosts
  expect(bodies[0]).toEqual(bodies[1]);
});
