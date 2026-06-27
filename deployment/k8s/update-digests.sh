#!/usr/bin/env bash
# Refresh the pinned image digests in chart/values.yaml from Docker Hub.
#
# Each managed line in values.yaml looks like:
#   image: hiversh/<repo>[:<tag>]@sha256:<digest>
# We re-resolve <repo>:<tag> (tag defaults to "latest") to its current manifest
# digest and rewrite the @sha256:... suffix in place. Lines without an @sha256
# pin are left untouched.
#
# Usage: deployment/k8s/update-digests.sh [path/to/values.yaml]
set -euo pipefail

VALUES="${1:-$(dirname "$0")/chart/values.yaml}"

digest_for() {
  local repo="$1" tag="$2" token
  token=$(curl -fsS "https://auth.docker.io/token?service=registry.docker.io&scope=repository:${repo}:pull" \
    | python3 -c 'import sys,json;print(json.load(sys.stdin)["token"])')
  curl -fsS -D - -o /dev/null \
    -H "Authorization: Bearer ${token}" \
    -H "Accept: application/vnd.docker.distribution.manifest.v2+json" \
    -H "Accept: application/vnd.docker.distribution.manifest.list.v2+json" \
    -H "Accept: application/vnd.oci.image.index.v1+json" \
    -H "Accept: application/vnd.oci.image.manifest.v1+json" \
    "https://registry-1.docker.io/v2/${repo}/manifests/${tag}" \
    | grep -i '^docker-content-digest:' | tr -d '\r' | awk '{print $2}'
}

# Pull every "image: hiversh/...@sha256:..." reference out of the file.
grep -oE 'hiversh/[A-Za-z0-9._/-]+(:[A-Za-z0-9._-]+)?@sha256:[0-9a-f]{64}' "$VALUES" \
  | sort -u | while read -r ref; do
  repo_tag="${ref%@*}"          # hiversh/claude:latest-microvm
  old_digest="${ref#*@}"        # sha256:...
  repo="${repo_tag%%:*}"        # hiversh/claude
  tag="latest"
  [[ "$repo_tag" == *:* ]] && tag="${repo_tag#*:}"

  new_digest="$(digest_for "$repo" "$tag")"
  if [[ "$new_digest" != sha256:* ]]; then
    echo "!! ${repo}:${tag} — could not resolve digest, skipping" >&2
    continue
  fi
  if [[ "$new_digest" == "$old_digest" ]]; then
    printf '   %-28s unchanged (%s)\n' "${repo}:${tag}" "${new_digest:7:12}"
    continue
  fi
  # Replace the specific old ref so we never touch an unrelated line.
  sed -i.bak "s|${repo_tag}@${old_digest}|${repo_tag}@${new_digest}|g" "$VALUES"
  printf ' ~ %-28s %s -> %s\n' "${repo}:${tag}" "${old_digest:7:12}" "${new_digest:7:12}"
done

rm -f "${VALUES}.bak"
