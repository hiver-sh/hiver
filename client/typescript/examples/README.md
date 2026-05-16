# Examples

Each file is a standalone script that imports from `../src` so you can
run them directly against your working copy without publishing the
package.

## Prerequisites

- Node 18+ (or Bun)
- A running Hive controller reachable at `http://localhost:9000` —
  `make up` from the repo root.

## Running

Use `tsx` to execute the `.ts` files directly:

```sh
npx tsx examples/quickstart.ts
npx tsx examples/apply-config.ts
npx tsx examples/files.ts
npx tsx examples/proxy.ts
npx tsx examples/resume-events.ts
```

To point at a non-default controller, set its URL on each
`getOrCreateSandbox` call:

```ts
await hive.getOrCreateSandbox(id, config, {
  controllerUrl: "http://controller.internal:9000",
});
```

## List of Examples

| File | Demonstrates |
| ---- | ------------ |
| `quickstart.ts` | Provision a sandbox, stream events, keep it alive with `ping`. Mirrors the README. |
| `apply-config.ts` | `GET /v1/config`, mutate, `PUT /v1/config`, and read back the diff the server applied. |
| `files.ts` | Upload a file into a mount and read it back through `GET /v1/file`. |
| `proxy.ts` | Use `sandbox.getUrl()` to talk to the HTTP service the sandbox image exposes. |
| `resume-events.ts` | Persist the last event id and resume the SSE stream after a restart. |
