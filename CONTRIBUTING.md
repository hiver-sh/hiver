# Contributing to Hiver

## Prerequisites

- **Go** — see the version in [`go.mod`](go.mod) (`go-version-file` is used in CI).
- **Docker** with Compose, for building images and running the stack.
- **Node.js 20+** — only if you're working on the TypeScript client.
- **Python 3.11+** — only if you're working on the Python client.

## Project layout

| Path | Description |
| --- | --- |
| [`cmd/`](cmd/) | Entry points for the binaries (`sandboxd`, `sbxfuse`, `sbxproxy`, `controller`, `sbxvsock`, `sbxguest`). |
| [`internal/`](internal/) | Core runtime packages. |
| [`api/`](api/) | API definitions. |
| [`cli/`](cli/) | The `hiver` command-line interface. |
| [`client/`](client/) | Language clients (TypeScript, Python). |
| [`docker/`](docker/) | Compose files and image definitions. |
| [`deployment/`](deployment/) | Kubernetes and GKE deployment manifests. |
| [`docs/`](docs/) | Documentation source. |
| [`benchmarks/`](benchmarks/) | Performance benchmarks. |
| [`scripts/`](scripts/) | Development and release helper scripts. |
| [`test/`](test/) | Unit and end-to-end tests. |

## Building

Build all binaries into `bin/`:

```sh
make build
```

Build a single binary:

```sh
make sandboxd
```

Build the Docker images:

```sh
make build-images
```

## Running the stack

```sh
make up      # start services via docker compose
make down    # stop services
```

Run `make help` to see all available targets.

## Testing

Run unit tests:

```sh
make test-unit
```

Run end-to-end tests:

```sh
make test-e2e
```

If you change the language clients, run their suites too:

```sh
# TypeScript
cd client/typescript && npm ci && npm test

# Python
cd client/python && pip install -e ".[dev]" && pytest tests/
```

CI runs the Go unit tests and both client suites on every push — see
[`.github/workflows/unit-tests.yaml`](.github/workflows/unit-tests.yaml).

### Test requirements for new code

New code must ship with automated tests that fully capture the scenario it
implements or fixes — enough that the test would fail without your change and
passes with it. Concretely:

- **Cover the behavior, not just the happy line.** Exercise the actual contract
  a caller depends on end-to-end, including the edge and failure paths your
  change introduces or touches.
- **A bug fix needs a regression test.** Add a test that reproduces the bug
  (and fails) on `main`, so the fix is what makes it pass and the bug can't
  silently return.
- **Don't paper over races with sleeps or bounded polling.** If correctness
  depends on some work having completed, assert against the guarantee the code
  provides (e.g. block until a durability barrier is reached), rather than
  waiting a few seconds and hoping. Timing-based waits flake under CI load and
  hide the real defect.
- **Put the test at the right level.** Prefer a unit test where the logic lives;
  add or extend an end-to-end test in [`test/e2e/`](test/e2e/) when the scenario
  only manifests across process or FUSE/network boundaries.

## Code style

- Format Go sources before committing:

  ```sh
  make fmt
  ```

  This runs `gofmt -s -w .`.

- If you change the API package, regenerate the generated code:

  ```sh
  make gen
  ```

## Submitting changes

1. Fork the repository and create a topic branch off `main`.
2. Make your change, keeping commits focused and descriptive.
3. Add automated tests that fully capture the scenario (see
   [Test requirements for new code](#test-requirements-for-new-code)).
4. Run `make fmt` and the relevant test targets — make sure they pass.
5. Push your branch and open a pull request against `main`.
6. Describe what changed and why. Link any related issues.

Please open an issue first for large or breaking changes so we can discuss the
approach before you invest significant effort.

## License

By contributing, you agree that your contributions will be licensed under the
project's [Apache 2.0](LICENSE) license.
