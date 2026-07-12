#!/usr/bin/env bash
# Create the immutable per-version tags for every sandbox image and record them
# in a lock JSON. Invoked from the Release workflow (tag-sandbox-images job)
# after the :latest[-microvm] manifests have been pushed.
#
# The image list and their source (:latest) tags are read straight from the CLI
# catalog cli/container-config/sandbox-images.json, so a new bundled image is
# picked up here with no edit. For each entry the versioned tag is derived with
# versioned_tag() below (mirrors deployment/k8s/pin-release-images.sh) and
# created from the :latest manifest with `docker buildx imagetools create`
# (a manifest-list copy, no re-pull).
#
# The lock JSON it writes (--lock) records exactly the tags created, e.g.
#   { "version": "0.1.31",
#     "images": {
#       "claude": { "image": "hiversh/claude:0.1.31",
#                   "microvm": "hiversh/claude:0.1.31-microvm" }, ... } }
# so downstream consumers (the CLI build's apply-image-tags.mjs) reference
# precisely what was pushed.
#
# Usage: scripts/tag-sandbox-images.sh <version> [--lock <path>] [--dry-run]
#   --dry-run  print the imagetools commands instead of running them (parity
#              checks / local runs without Docker or registry access).
set -euo pipefail

VERSION="${1:?usage: tag-sandbox-images.sh <version> [--lock <path>] [--dry-run]}"
shift

LOCK=""
DRY_RUN=0
while [[ $# -gt 0 ]]; do
  case "$1" in
    --lock) LOCK="${2:?--lock needs a path}"; shift 2 ;;
    --dry-run) DRY_RUN=1; shift ;;
    *) echo "unknown arg: $1" >&2; exit 2 ;;
  esac
done

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
CATALOG="${ROOT}/cli/container-config/sandbox-images.json"

# Map a source tag to the versioned tag the release publishes:
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

# repo[:tag] -> repo:<versioned tag>
versioned_ref() {
  local ref="$1" repo tag=""
  repo="${ref%%:*}"
  [[ "$ref" == *:* ]] && tag="${ref#*:}"
  echo "${repo}:$(versioned_tag "$tag")"
}

# Copy a :latest[-microvm] manifest list to its versioned tag.
retag() {
  local src="$1" dst="$2"
  if [[ "$DRY_RUN" == 1 ]]; then
    echo "docker buildx imagetools create --tag ${dst} ${src}"
  else
    docker buildx imagetools create --tag "${dst}" "${src}"
  fi
}

# Accumulate the lock as {name: {image, microvm}} then wrap with the version.
lock_images="{}"
while IFS=$'\t' read -r name image microvm; do
  vimage="$(versioned_ref "$image")"
  vmicrovm="$(versioned_ref "$microvm")"
  retag "$image" "$vimage"
  retag "$microvm" "$vmicrovm"
  lock_images="$(jq \
    --arg n "$name" --arg i "$vimage" --arg m "$vmicrovm" \
    '.[$n] = {image: $i, microvm: $m}' <<<"$lock_images")"
done < <(jq -r 'to_entries[] | [.key, .value.image, .value.microvm] | @tsv' "$CATALOG")

lock_json="$(jq -n --arg v "$VERSION" --argjson imgs "$lock_images" \
  '{version: $v, images: $imgs}')"

if [[ -n "$LOCK" ]]; then
  printf '%s\n' "$lock_json" >"$LOCK"
  echo "wrote lock -> $LOCK" >&2
else
  printf '%s\n' "$lock_json"
fi
