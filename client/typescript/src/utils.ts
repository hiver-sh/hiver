import { EgressRule, SandboxConfig } from "./schemas";

export function allowedPythonPackages(...packages: string[]): EgressRule[] {
  return [
    {
      access: "allow",
      host: "pypi.org",
      paths: packages.map((pyPackage) => `/simple/${pyPackage}/`),
    },
    {
      access: "allow",
      host: "files.pythonhosted.org",
    },
  ];
}

export function allowedNpmPackages(...packages: string[]): EgressRule[] {
  return packages.map((packageName: string) => {
    return {
      access: "allow",
      host: "registry.npmjs.org",
      paths: [`/${packageName}`, `/${packageName}/*`],
    };
  });
}

/**
 * Build egress rules that let an agent create and reach a single nested sandbox
 * named `sandboxKey` through the gateway, using a fixed `config` the agent
 * cannot tamper with.
 *
 * The base rules returned are:
 *  - a POST to create the sandbox by key, whose request body is replaced with
 *    `config` (body_strategy "replace") so the agent cannot influence what gets
 *    created.
 *  - a passthrough rule for that sandbox's gateway proxy routes.
 *
 * Pass `allowedDirs` to also open the nested sandbox's file API under specific
 * directories. Each entry allows `POST`/`GET`/`DELETE` on the file endpoint's
 * `.../file/<dir>/**` glob, so the agent can seed and read back files there
 * without reaching the rest of the sandbox's filesystem. When omitted, no file
 * rules are added. Entries are matched relative to the file endpoint, so pass
 * agent paths without a leading slash (e.g. `"workspace/inputs"`).
 *
 * Rules are emitted for both the Docker (`gateway`) and k8s (`gateway.hiver`)
 * gateway hosts. Add the returned rules to the outer `SandboxConfig.egress`.
 */
export function allowSandbox(
  sandboxKey: string,
  config: SandboxConfig,
  allowedDirs: string[] | undefined = undefined,
): EgressRule[] {
  const allowedPaths = allowedDirs?.map((dir) => `/sandbox/*/v1/${sandboxKey}/file/${dir.replace(/^\//, "")}/**`);
  const paths: EgressRule[] = allowedDirs ? [
    // docker
    {
      access: "allow",
      host: "gateway",
      paths: allowedPaths,
    },
    // k8s
    {
      access: "allow",
      host: "gateway.hiver",
      paths: allowedPaths,
    },
  ] : [];
  return [
    // docker
    {
      access: "allow",
      host: "gateway",
      paths: [`/v1/sandboxes/${sandboxKey}`],
      methods: ["POST"],
      override: {
        body: { ...config },
        body_strategy: "replace",
      },
    },
    {
      access: "allow",
      host: "gateway",
      paths: [`/sandbox/*/v1/${sandboxKey}/proxy/**`],
    },
    // k8s
    {
      access: "allow",
      host: "gateway.hiver",
      paths: [`/v1/sandboxes/${sandboxKey}`],
      methods: ["POST"],
      override: {
        body: { ...config },
        body_strategy: "replace",
      },
    },
    {
      access: "allow",
      host: "gateway.hiver",
      paths: [`/sandbox/*/v1/${sandboxKey}/proxy/**`],
    },
    ...paths,
  ];
}
