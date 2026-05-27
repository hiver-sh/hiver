#!/bin/bash
set -e

mkdir -p /run/sshd /home/agent/.ssh /home/agent/.claude

ssh-keygen -A

if [ -n "$CLAUDE_CODE_OAUTH_TOKEN" ]; then
  # Seed ~/.claude.json with auth state. Written during startup before the API
  # call completes, so a timeout is expected and acceptable.
  # Workaround for https://github.com/anthropics/claude-code/issues/8938
  su -s /bin/bash agent \
    -c "CLAUDE_CODE_OAUTH_TOKEN='$CLAUDE_CODE_OAUTH_TOKEN' timeout 30 claude -p ok" \
    2>/dev/null || true

  # Set hasCompletedOnboarding in settings.json
  node -e "
    const fs = require('fs');

    const configPath = '/home/agent/.claude.json';
    const extra = process.env.CLAUDE_CONFIG ? JSON.parse(process.env.CLAUDE_CONFIG) : {};
    let config = {};
    try { config = JSON.parse(fs.readFileSync(configPath, 'utf8')); } catch (e) {}
    config.hasCompletedOnboarding = true;
    if (extra.oauthAccount) config.oauthAccount = extra.oauthAccount;
    if (extra.lastOnboardingVersion) config.lastOnboardingVersion = extra.lastOnboardingVersion;
    const trusted = { hasTrustDialogAccepted: true, hasCompletedProjectOnboarding: true };
    config.projects = {
      '/workspace': trusted,
      '/home/agent': trusted,
      ...config.projects,
      ...extra.projects,
    };
    fs.writeFileSync(configPath, JSON.stringify(config, null, 2) + '\n');
  "
  chown agent:agent \
    /home/agent/.claude.json 2>/dev/null || true
fi

if [ "${AGENT:-claude-code}" = "codex" ]; then
  mkdir -p /home/agent/.codex
  node -e "
    const fs = require('fs');
    const model = process.env.MODEL || 'o4-mini';
    const config = { model };
    fs.writeFileSync('/home/agent/.codex/config.json', JSON.stringify(config, null, 2) + '\n');
  "
  chown -R agent:agent /home/agent/.codex
fi

# Propagate token and agent selection into SSH sessions
: > /home/agent/.ssh/environment
for var in CLAUDE_CODE_OAUTH_TOKEN OPENAI_API_KEY GEMINI_API_KEY GITHUB_TOKEN MODEL AGENT NODE_EXTRA_CA_CERTS; do
  [ -n "${!var}" ] && echo "$var=${!var}" >> /home/agent/.ssh/environment
done

chown agent:agent /home/agent/.ssh/environment
chmod 600 /home/agent/.ssh/environment

case "${AGENT:-claude-code}" in
  codex)
    AGENT_CMD="codex"
    ;;
  gemini)
    AGENT_CMD="gemini"
    ;;
  copilot)
    AGENT_CMD="copilot"
    ;;
  *)
    AGENT_CMD="claude"
    ;;
esac

mkdir -p /home/agent/.config/zellij/layouts

cat > /home/agent/.config/zellij/config.kdl << 'EOF'
pane_frames false
default_layout "agent"
show_release_notes false
show_startup_tips false
on_force_close "detach"
EOF

cat > /home/agent/.config/zellij/layouts/agent.kdl << EOF
layout {
    pane command="bash" close_on_exit=false {
        args "-c" "cd /workspace && $AGENT_CMD || exec bash"
    }
}
EOF

chown -R agent:agent /home/agent/.config

exec /usr/sbin/sshd -D
