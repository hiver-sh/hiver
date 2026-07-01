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
 * Two rules are returned:
 *  - a POST to create the sandbox by key, whose request body is replaced with
 *    `config` (body_strategy "replace") so the agent cannot influence what gets
 *    created.
 *  - a passthrough rule for that sandbox's gateway proxy routes.
 *
 * Add the returned rules to the outer `SandboxConfig.egress`.
 */
export function allowSandbox(
  sandboxKey: string,
  config: SandboxConfig,
): EgressRule[] {
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
  ];
}
