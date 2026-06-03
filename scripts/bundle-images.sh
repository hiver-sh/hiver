#!/usr/bin/env bash
set -euo pipefail

SANDBOX_IMAGE="${1}"
SANDBOX_RUNTIME_TAG="${2}"

TMPDIR=$(mktemp -d)
trap 'rm -rf "${TMPDIR}"' EXIT

docker save "${SANDBOX_IMAGE}" > "${TMPDIR}/sandbox.tar"

docker build \
  -f docker/bundler.Dockerfile \
  --build-context sandbox-tar="${TMPDIR}" \
  -t "${SANDBOX_RUNTIME_TAG}" \
  .
