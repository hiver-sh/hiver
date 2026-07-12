#!/usr/bin/env bash
# Rewrite chart/values.yaml image references to the *versioned* tags the Release
# workflow publishes, pinned to their current manifest digests. Run at release
# time (CI) against a fresh checkout; the result is packaged into the released
# Helm chart so an installed chart pins exactly the images published under the
# same version — never a drifting `:latest`.
#
# The versioned tag mirrors the naming in .github/workflows/release.yaml
# (push-runtime-images / tag-sandbox-images), for VERSION=X.Y.Z:
#   hiversh/controller@sha256:...                 -> hiversh/controller:X.Y.Z@sha256:<d>
#   hiversh/gateway@sha256:...                    -> hiversh/gateway:X.Y.Z@sha256:<d>
#   hiversh/claude:latest-microvm@sha256:...      -> hiversh/claude:X.Y.Z-microvm@sha256:<d>
#   hiversh/python:3.13-alpine-microvm@sha256:... -> hiversh/python:X.Y.Z-3.13-alpine-microvm@sha256:<d>
#   hiversh/node:alpine-microvm@sha256:...        -> hiversh/node:X.Y.Z-alpine-microvm@sha256:<d>
#
# Usage: deployment/k8s/pin-release-images.sh <version> [path/to/values.yaml]
set -euo pipefail

VERSION="${1:?usage: pin-release-images.sh <version> [values.yaml]}"
VALUES="${2:-$(dirname "$0")/chart/values.yaml}"

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

# Map a source tag to the versioned tag the release pipeline publishes:
#   (no tag)          -> X.Y.Z
#   contains "latest" -> substitute "latest" with X.Y.Z  (latest-microvm -> X.Y.Z-microvm)
#   otherwise         -> X.Y.Z-<tag>  (3.13-alpine-microvm -> X.Y.Z-3.13-alpine-microvm)
versioned_tag() {
  local tag="$1"
  if [[ -z "$tag" ]]; then echo "$VERSION"
  elif [[ "$tag" == *latest* ]]; then echo "${tag/latest/$VERSION}"
  else echo "${VERSION}-${tag}"
  fi
}

# Process substitution (not a pipe) so `set -e` failures abort the whole script.
while read -r ref; do
  repo_tag="${ref%@*}"          # hiversh/claude:latest-microvm  |  hiversh/controller
  old_digest="${ref#*@}"        # sha256:...
  repo="${repo_tag%%:*}"        # hiversh/claude
  tag=""
  [[ "$repo_tag" == *:* ]] && tag="${repo_tag#*:}"

  vtag="$(versioned_tag "$tag")"
  new_digest="$(digest_for "$repo" "$vtag")"
  if [[ "$new_digest" != sha256:* ]]; then
    echo "!! ${repo}:${vtag} — could not resolve digest" >&2
    exit 1
  fi
  new_ref="${repo}:${vtag}@${new_digest}"
  # Replace the exact old ref (repo[:tag]@olddigest) so unrelated lines are safe.
  sed -i.bak "s|${repo_tag}@${old_digest}|${new_ref}|g" "$VALUES"
  printf ' ~ %-46s -> %s\n' "$ref" "$new_ref"
done < <(grep -oE 'hiversh/[A-Za-z0-9._/-]+(:[A-Za-z0-9._-]+)?@sha256:[0-9a-f]{64}' "$VALUES" | sort -u)

rm -f "${VALUES}.bak"
