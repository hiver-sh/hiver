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

# .env.local lives in the repo root, gitignored. Devs can stash secrets
# there (HIVE_GDRIVE_* tokens, registry creds, etc.) so they're not
# re-prompted every run. Sourced before any backend-specific block so
# downstream env-var fallbacks see the values.
env_local="${module_root}/.env.local"
if [[ -f "${env_local}" ]]; then
  echo "==> Sourcing ${env_local}"
  set -a  # auto-export everything we source
  # shellcheck disable=SC1090
  source "${env_local}"
  set +a
fi

if [[ ! -d "${fixture_dir}" ]]; then
  echo "fixture not found: ${fixture_dir}" >&2
  exit 1
fi
if [[ ! -f "${fixture_dir}/Dockerfile" || ! -f "${fixture_dir}/spec.yaml" ]]; then
  echo "fixture is missing Dockerfile and/or spec.yaml: ${fixture_dir}" >&2
  exit 1
fi

agent_image="sandbox-${fixture}:e2e"
container_name="sandbox-pod-${fixture}"

# Detect the FS backend from the fixture's spec. Cheap grep instead of
# a YAML dependency — the file shape is well-known, every fixture
# carries `backend: <name>` on its own line under `fs:`.
spec_backend="$(awk '/^[[:space:]]*backend:[[:space:]]/ { gsub(/"|,/, "", $2); print $2; exit }' "${fixture_dir}/spec.yaml")"

# Per-backend env-var passthrough into the container. spec.go falls
# back from blank spec fields to these env vars, so a checked-in
# spec.yaml can ship without secrets.
backend_env_args=()
if [[ "${spec_backend}" == "gdrive" ]]; then
  # If we don't have an access token yet (and no service account
  # either), run the interactive OAuth helper. It writes
  # `export HIVE_GDRIVE_…` lines to stdout that we eval into this
  # shell; everything else (prompts, browser URL, folder picker)
  # goes to stderr.
  if [[ -z "${HIVE_GDRIVE_ACCESS_TOKEN:-}" && -z "${HIVE_GDRIVE_SERVICE_ACCOUNT_JSON:-}" ]]; then
    echo "==> gdrive backend: no HIVE_GDRIVE_ACCESS_TOKEN set; running OAuth setup"
    exports="$(go run "${module_root}/test/e2e/hive-gdrive-setup")"
    eval "${exports}"

    # Persist HIVE_GDRIVE_* lines to .env.local so subsequent runs skip
    # the OAuth flow. We normalize the helper's `export NAME=value` lines
    # to bare `NAME=value` so the file stays in standard dotenv format
    # (the source-with-`set -a` block at the top reads either form).
    # Existing HIVE_GDRIVE_* lines (in either form) are replaced; every
    # other line in the file is preserved untouched.
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
  fi
  for v in HIVE_GDRIVE_ACCESS_TOKEN HIVE_GDRIVE_REFRESH_TOKEN \
           HIVE_GDRIVE_CLIENT_ID HIVE_GDRIVE_CLIENT_SECRET \
           HIVE_GDRIVE_SERVICE_ACCOUNT_JSON HIVE_GDRIVE_FOLDER_ID; do
    # `-e VAR` (no value) forwards from the host environment when set,
    # silently omits when not.
    backend_env_args+=(-e "${v}")
  done
  echo "==> gdrive backend: forwarding HIVE_GDRIVE_* env vars"
fi

echo "==> Building sandbox-runtime"
docker build -t sandbox-runtime "${module_root}"

echo "==> Building agent image: ${agent_image}"
docker build -t "${agent_image}" "${fixture_dir}"

# docker save → tarball that sandboxd will unpack inside the pod.
audit_dir="$(mktemp -d -t sandbox-audit-XXXXXX)"
agent_tar="${audit_dir}/agent.tar"
echo "==> Saving agent image to ${agent_tar}"
docker save -o "${agent_tar}" "${agent_image}"

# Tear down any previous run of the same fixture.
if docker inspect "${container_name}" >/dev/null 2>&1; then
  echo "==> Removing stale container ${container_name}"
  docker rm -f "${container_name}" >/dev/null
fi

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
  -p 18000:18000 \
  ${backend_env_args[@]+"${backend_env_args[@]}"} \
  -v "${audit_dir}:/audit-out" \
  -v "${agent_tar}:/mnt/agent.tar:ro" \
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
ops_file="${audit_dir}/agent-ops.txt"
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
  proxy audit   tail -f ${audit_dir}/proxy.log
  fuse audit    tail -f ${audit_dir}/fuse.log
  sandbox CA    ${audit_dir}/sandbox-ca.crt
  ingress       curl -d 'hi from host' http://localhost:18000/hello
  exec cmd      curl -d 'uname -a; ls /workspace' http://localhost:18000/exec
  stop          docker kill ${container_name}

Audit dir lives in ${audit_dir} and is bind-mounted at /audit-out inside the pod.
EOF
