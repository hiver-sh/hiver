#!/bin/bash
set -e

mkdir -p /run/sshd /home/claude-agent/.ssh /home/claude-agent/.claude

ssh-keygen -A

if [ -n "$CLAUDE_CODE_OAUTH_TOKEN" ]; then
  # Write credentials file (access token only — no refresh token so the sandbox
  # cannot rotate the OAuth pair and invalidate the host session)
  printf '{"claudeAiOauth":{"accessToken":"%s"}}\n' "$CLAUDE_CODE_OAUTH_TOKEN" \
    > /home/claude-agent/.claude/.credentials.json
  chown claude-agent:claude-agent /home/claude-agent/.claude/.credentials.json
  chmod 600 /home/claude-agent/.claude/.credentials.json

  # Seed ~/.claude.json with auth state. Written during startup before the API
  # call completes, so a timeout is expected and acceptable.
  # Workaround for https://github.com/anthropics/claude-code/issues/8938
  su -s /bin/bash claude-agent \
    -c "CLAUDE_CODE_OAUTH_TOKEN='$CLAUDE_CODE_OAUTH_TOKEN' timeout 30 claude -p ok" \
    2>/dev/null || true

  # Set hasCompletedOnboarding in settings.json
  node -e "
    const fs = require('fs');

    const settingsPath = '/home/claude-agent/.claude/settings.json';
    let settings = {};
    try { settings = JSON.parse(fs.readFileSync(settingsPath, 'utf8')); } catch (e) {}
    settings.hasCompletedOnboarding = true;
    fs.writeFileSync(settingsPath, JSON.stringify(settings, null, 2) + '\n');

    const configPath = '/home/claude-agent/.claude.json';
    const extra = process.env.CLAUDE_CONFIG ? JSON.parse(process.env.CLAUDE_CONFIG) : {};
    let config = {};
    try { config = JSON.parse(fs.readFileSync(configPath, 'utf8')); } catch (e) {}
    config.hasCompletedOnboarding = true;
    if (extra.oauthAccount) config.oauthAccount = extra.oauthAccount;
    if (extra.lastOnboardingVersion) config.lastOnboardingVersion = extra.lastOnboardingVersion;
    const trusted = { hasTrustDialogAccepted: true, hasCompletedProjectOnboarding: true };
    config.projects = {
      '/workspace': trusted,
      '/home/claude-agent': trusted,
      ...config.projects,
      ...extra.projects,
    };
    fs.writeFileSync(configPath, JSON.stringify(config, null, 2) + '\n');
  "
  chown claude-agent:claude-agent \
    /home/claude-agent/.claude/settings.json \
    /home/claude-agent/.claude.json 2>/dev/null || true
fi

# Propagate token into SSH sessions
: > /home/claude-agent/.ssh/environment
for var in CLAUDE_CODE_OAUTH_TOKEN; do
  [ -n "${!var}" ] && echo "$var=${!var}" >> /home/claude-agent/.ssh/environment
done
chown claude-agent:claude-agent /home/claude-agent/.ssh/environment
chmod 600 /home/claude-agent/.ssh/environment

su -s /bin/bash claude-agent -c 'tmux new-session -d -s claude "cd /workspace && claude"'

exec /usr/sbin/sshd -D
