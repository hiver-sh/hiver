#!/usr/bin/env bash
set -euo pipefail

SANDBOX_IMAGE="${1:-hive-mcp-server}"
SANDBOX_RUNTIME_TAG="${2:-hive-sandbox-bundle}"

TMPDIR=$(mktemp -d)
trap 'rm -rf "${TMPDIR}"' EXIT

docker save "${SANDBOX_IMAGE}" > "${TMPDIR}/sandbox.tar"

docker build \
  -f docker/sandbox-bundle.Dockerfile \
  --build-context sandbox-tar="${TMPDIR}" \
  -t "${SANDBOX_RUNTIME_TAG}" \
  .
