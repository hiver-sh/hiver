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
  --privileged \
  --device /dev/fuse \
  --add-host upstream-allowed:host-gateway \
  --add-host upstream-denied:host-gateway \
  -p 18000:18000 \
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
docker logs "${container_name}" 2>&1 \
  | sed -n 's/.*sandboxd: agent op | //p' \
  > "${ops_file}"

echo
echo "==> Observed agent ops:"
cat "${ops_file}"

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
