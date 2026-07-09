#!/bin/sh
# Entrypoint wrapper for the OpenClaw gateway image.
#
# The build bakes the Hiver sandbox settings into openclaw.json, but this image
# is provisioned by a layer that injects its own openclaw.json at create time,
# which drops those settings (and the hiver-sandbox plugin entry). Re-assert them
# here on every start — after provisioning has written the config but before the
# gateway boots — so exec/process and the filesystem tools actually route into a
# Hiver sandbox instead of running on the gateway host. `config set` is
# idempotent, so this is a no-op when the values are already present.
set -e

openclaw config set agents.defaults.sandbox.mode all
openclaw config set agents.defaults.sandbox.backend hiver
openclaw config set plugins.entries.hiver-sandbox.enabled true
# Keep OpenClaw's built-in browser plugin off. It ships enabled and would drive
# the gateway host's own browser engine instead of the sandbox's Chrome; we ship
# the agent-base CDP browser skill (in ~/.openclaw/skills) instead. Re-asserted
# here because provisioning's openclaw.json would otherwise re-enable it.
openclaw config set plugins.entries.browser.enabled false
# NOTE: do NOT set `plugins.allow` to pin the hiver-sandbox plugin. A non-empty
# plugins.allow is a strict global allowlist — it blocks every other plugin,
# including bundled model providers (anthropic, etc.), so model resolution fails
# with "Unknown model" and the agent can't run. The "loaded without provenance"
# warning for a path-installed plugin is cosmetic; leave the allowlist empty.

# Default the agent to Anthropic (Claude) instead of the built-in openai/gpt-5.5.
# This needs three things or turns fail "Unknown model": (1) the model set below,
# (2) the anthropic provider plugin enabled (it ships disabled), (3) an anthropic
# auth profile. --merge keeps any models provisioning added.
openclaw config set agents.defaults.models '{"anthropic/claude-sonnet-4-6":{},"anthropic/claude-opus-4-8":{}}' --strict-json --merge
openclaw config set agents.defaults.model.primary anthropic/claude-sonnet-4-6
openclaw config set plugins.entries.anthropic.enabled true
# Register the anthropic API key from the runtime env as an auth profile so the
# provider can authenticate. Best-effort; a missing key just leaves it unset.
if [ -n "$ANTHROPIC_API_KEY" ]; then
  printf '%s\n' "$ANTHROPIC_API_KEY" \
    | openclaw models auth paste-api-key --provider anthropic --profile-id anthropic:default \
    >/dev/null 2>&1 || true
fi

# Disable the Control UI / webchat "device pairing required" gate. The gateway
# otherwise makes each new browser device (client id CONTROL_UI, including
# webchat mode) go through owner-approved device pairing before it can connect;
# in this single-tenant sandbox there is no owner UI to approve from, so every
# connect fails with "device pairing required". This tells the gateway to rely
# on the shared password alone (already required by `--auth password`) and skip
# device identity/pairing checks for the operator Control UI.
openclaw config set gateway.controlUi.dangerouslyDisableDeviceAuth true

# Clear a stale workspace attestation left over an empty workspace. This image's
# workspace lives on the sandbox's ephemeral overlay; when it is reset (fresh
# start, snapshot/resume) the BOOTSTRAP.md contents can be gone while the
# attestation marker survives, which makes the gateway throw WorkspaceVanishedError
# and refuse to reseed. Only clear when the workspace is actually empty, so a
# genuinely-populated workspace still trips the guard.
ws="${HOME:-/home/agent}/.openclaw/workspace"
att="${HOME:-/home/agent}/.openclaw/workspace-attestations"
if [ -d "$att" ] && [ -z "$(ls -A "$ws" 2>/dev/null)" ]; then
  echo "entrypoint: workspace $ws is empty; clearing stale attestations to allow reseed"
  rm -f "$att"/*.attested 2>/dev/null || true
fi

exec openclaw gateway run --port 18789 --bind lan --auth password "$@"
