# Contributing to Hiver

Thanks for your interest in contributing! Hiver is Chrome DevTools for agents — a
runtime that runs agents autonomously with controlled network access, auditable
file operations, and full execution visibility. This guide covers how to set up
your environment, make changes, and submit them.

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
| [`client/`](client/) | Language clients (TypeScript, Python). |
| [`inspector/`](inspector/) | DevTools inspector UI. |
| [`docker/`](docker/) | Compose files and image definitions. |
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
3. Run `make fmt` and the relevant test targets — make sure they pass.
4. Push your branch and open a pull request against `main`.
5. Describe what changed and why. Link any related issues.

Please open an issue first for large or breaking changes so we can discuss the
approach before you invest significant effort.

## License

By contributing, you agree that your contributions will be licensed under the
project's [Apache 2.0](LICENSE) license.
