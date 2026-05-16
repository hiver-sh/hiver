#!/usr/bin/env bash
#
# Bring up the sandbox-pod against a fixture from test/e2e/fixtures/.
#
# Usage:
#   ./test/e2e/run-fixture.sh [fixture-name]
#
# Default fixture is agent-python.

set -euo pipefail

fixture="${1:-agent-python}"
script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
module_root="$(cd "${script_dir}/../.." && pwd)"
fixture_dir="${script_dir}/fixtures/${fixture}"


env_local="${module_root}/.env.local"
env_local_kvs=()
load_env_local() {
  env_local_kvs=()
  [[ -f "${env_local}" ]] || return 0
  while IFS= read -r line; do
    [[ -z "${line}" || "${line}" =~ ^[[:space:]]*# ]] && continue
    if [[ "${line}" =~ ^([A-Za-z_][A-Za-z0-9_]*)=\'(.*)\'$ ]]; then
      env_local_kvs+=("${BASH_REMATCH[1]}=${BASH_REMATCH[2]}")
    fi
  done < "${env_local}"
}
env_local_has() {
  local target="$1" kv
  for kv in "${env_local_kvs[@]+"${env_local_kvs[@]}"}"; do
    [[ "${kv}" == "${target}="?* ]] && return 0
  done
  return 1
}
load_env_local
if [[ ${#env_local_kvs[@]} -gt 0 ]]; then
  echo "==> Loaded ${#env_local_kvs[@]} entries from ${env_local}"
fi

if [[ ! -d "${fixture_dir}" ]]; then
  echo "fixture not found: ${fixture_dir}" >&2
  exit 1
fi
if [[ ! -f "${fixture_dir}/spec.yaml" ]]; then
  echo "fixture is missing spec.yaml: ${fixture_dir}" >&2
  exit 1
fi

# Resolve image from spec.yaml. `image:` may be either a directory
# containing a Dockerfile (e.g. `.`) or the Dockerfile itself
# (e.g. `../../../../docker/mcpserver.Dockerfile`).
image_value="$(awk '
  /^image:/ { print $2; exit }
' "${fixture_dir}/spec.yaml")"
image_value="${image_value:-.}"
image_path="$(cd "${fixture_dir}" && cd "$(dirname "${image_value}")" && pwd)/$(basename "${image_value}")"
if [[ -d "${image_path}" ]]; then
  dockerfile="${image_path}/Dockerfile"
elif [[ -f "${image_path}" ]]; then
  dockerfile="${image_path}"
else
  echo "image path not found: ${image_path}" >&2
  exit 1
fi
if [[ ! -f "${dockerfile}" ]]; then
  echo "sandbox Dockerfile not found: ${dockerfile}" >&2
  exit 1
fi
sandbox_dir="$(dirname "${dockerfile}")"
build_context="${sandbox_dir}"
case "${image_value}" in
  ..*) build_context="${module_root}" ;;
esac

sandbox_image="sandbox-${fixture}:e2e"
container_name="sandbox-pod-${fixture}"

# Detect the FS backend from the fixture's spec. Cheap parse instead of
# a YAML dependency — the file shape is well-known, every fixture
# carries `backend: <name>` on its own line under `fs:`. Accepts both
# the bare key (`backend: gdrive`) and the list-entry form
# (`- backend: gdrive`); first match wins, which matches sandboxd's
# "remote env vars apply to any gdrive entry" behavior.
spec_backend="$(awk '
  {
    for (i=1; i<=NF; i++) {
      if ($i == "backend:") {
        v = $(i+1); gsub(/"|,/, "", v); print v; exit
      }
    }
  }
' "${fixture_dir}/spec.yaml")"

if [[ "${spec_backend}" == "gdrive" ]]; then
  # Run the OAuth helper if neither an access token nor a service
  # account is already in .env.local. The helper writes
  # `export KEY='value'` lines to stdout (everything else — prompts,
  # browser URL, folder picker — goes to stderr). It reads OAuth
  # client creds from its own env, so we forward .env.local through
  # `env` rather than sourcing.
  if ! env_local_has HIVE_GDRIVE_ACCESS_TOKEN && ! env_local_has HIVE_GDRIVE_SERVICE_ACCOUNT_JSON; then
    echo "==> gdrive backend: no HIVE_GDRIVE_ACCESS_TOKEN or HIVE_GDRIVE_SERVICE_ACCOUNT_JSON in ${env_local}; running OAuth setup"
    exports="$(env "${env_local_kvs[@]+"${env_local_kvs[@]}"}" \
      go run "${module_root}/test/e2e/setup/gdrive")"

    # Persist new HIVE_GDRIVE_* values to .env.local in `KEY='value'`
    # form, replacing any prior HIVE_GDRIVE_* lines. The helper's
    # `export KEY='value'` lines just need the `export ` prefix
    # stripped to land in our format.
    umask 077
    if [[ -f "${env_local}" ]]; then
      grep -Ev '^[[:space:]]*(export[[:space:]]+)?HIVE_GDRIVE_' "${env_local}" \
        > "${env_local}.tmp" || true
      mv "${env_local}.tmp" "${env_local}"
    fi
    printf '%s\n' "${exports}" \
      | sed -E 's/^[[:space:]]*export[[:space:]]+//' \
      >> "${env_local}"
    chmod 600 "${env_local}"
    echo "==> Wrote HIVE_GDRIVE_* tokens to ${env_local} (gitignored, mode 600)"

    # Reload so the new values are visible to env_local_kvs below.
    load_env_local
  fi
fi

echo "==> Building sandbox-runtime"
docker build -t sandbox-runtime -f "${module_root}/docker/sandbox.Dockerfile" "${module_root}"

echo "==> Building sandbox image: ${sandbox_image}"

docker build -t "${sandbox_image}" -f "${dockerfile}" "${build_context}"


# docker save → tarball that sandboxd will unpack inside the pod.
staging_dir="$(mktemp -d -t sandbox-staging-XXXXXX)"
sandbox_tar="${staging_dir}/sandbox.tar"
echo "==> Saving sandbox image to ${sandbox_tar}"
docker save -o "${sandbox_tar}" "${sandbox_image}"

# Tear down any previous run of the same fixture.
if docker inspect "${container_name}" >/dev/null 2>&1; then
  echo "==> Removing stale container ${container_name}"
  docker rm -f "${container_name}" >/dev/null
fi

# Each .env.local entry becomes a `-e KEY=VALUE` pair so docker
# receives the value verbatim (handles spaces, quotes, JSON blobs).
docker_env_args=()
for kv in "${env_local_kvs[@]+"${env_local_kvs[@]}"}"; do
  docker_env_args+=(-e "${kv}")
done

echo "==> Starting sandbox-pod (detached): ${container_name}"
docker run -d --rm \
  --name "${container_name}" \
  --device /dev/fuse \
  --cap-add SYS_ADMIN --cap-add NET_ADMIN --cap-add MKNOD \
  --cap-add SYS_CHROOT --cap-add SETPCAP --cap-add SETFCAP \
  --cap-add SETUID --cap-add SETGID \
  --cap-add DAC_READ_SEARCH --cap-add FOWNER --cap-add CHOWN \
  --security-opt apparmor=unconfined \
  --security-opt seccomp=unconfined \
  -v /sys/fs/cgroup:/sys/fs/cgroup:rw \
  --add-host upstream-allowed:host-gateway \
  --add-host upstream-denied:host-gateway \
  -p 8080:8080 \
  ${docker_env_args[@]+"${docker_env_args[@]}"} \
  -v "${sandbox_tar}:/mnt/sandbox.tar:ro" \
  -v "${fixture_dir}/spec.yaml:/mnt/spec.yaml:ro" \
  sandbox-runtime \
  --spec /mnt/spec.yaml >/dev/null

# Wait until the agent's probes have settled. We poll docker logs for
# "agent op |" lines until the count stops growing — that's a reliable
# proxy for "the agent has reached its sleep loop". Cap at 30 s.
echo "==> Waiting for agent ops to settle"
prev=-1
for _ in $(seq 1 30); do
  count="$(docker logs "${container_name}" 2>&1 | grep -c 'agent op |' || true)"
  if [[ "${count}" -gt 0 && "${count}" -eq "${prev}" ]]; then
    break
  fi
  prev="${count}"
  sleep 1
done

# Capture the operation log and print to stdout. The leading
# "sandboxd: " prefix is stripped so each line reads as a single op.
ops_file="${staging_dir}/agent-ops.txt"
# `|| true` so a crashed/removed container doesn't make `set -e + pipefail`
# silently exit before we print diagnostics below.
{ docker logs "${container_name}" 2>&1 || true; } \
  | sed -n 's/.*sandboxd: agent op | //p' \
  > "${ops_file}"

# If the container died, surface its full logs and a clear "crashed" banner
# instead of pretending everything is fine.
container_state="$(docker inspect -f '{{.State.Status}}' "${container_name}" 2>/dev/null || echo missing)"
if [[ "${container_state}" != "running" ]]; then
  echo
  echo "==> Sandbox-pod container is not running (state=${container_state}). Last 100 log lines:"
  docker logs --tail 100 "${container_name}" 2>&1 || echo "(container removed; no logs available)"
fi

echo
echo "==> Observed agent ops:"
cat "${ops_file}"

cat <<EOF

sandbox-pod is still running.

  follow logs   docker logs -f ${container_name}
  shell         docker exec -it ${container_name} bash
  proxy audit   docker logs -f ${container_name} 2>&1 | grep 'agent op | proxy'
  fuse audit    docker logs -f ${container_name} 2>&1 | grep 'agent op | fuse'
  sandbox CA    docker exec ${container_name} cat /run/sandboxd/sandbox-ca.crt
  ingress       curl -d 'hi from host' http://localhost:8080/v1/sandbox/hello
  exec cmd      curl -d 'uname -a; ls /workspace' http://localhost:8080/v1/sandbox/exec
  stop          docker kill ${container_name}

Host-side staging (sandbox.tar, ops log) lives in ${staging_dir}.
EOF

if [[ "${fixture}" == "mcp-server" ]]; then
  inspector_url="http://localhost:8080/v1/sandbox"
  echo "==> Starting MCP inspector:"
  npx @modelcontextprotocol/inspector --server-url "${inspector_url}"
fi
