#!/bin/bash
set -e

mkdir -p /run/sshd /home/agent/.ssh /home/agent/.claude

ssh-keygen -A

# Propagate token and agent selection into SSH sessions
: > /home/agent/.ssh/environment
for var in CLAUDE_CODE_OAUTH_TOKEN OPENAI_API_KEY GEMINI_API_KEY GITHUB_TOKEN MODEL AGENT PROMPT NODE_EXTRA_CA_CERTS; do
  [ -n "${!var}" ] && echo "$var=${!var}" >> /home/agent/.ssh/environment
done

echo "COLORTERM=truecolor" >> /home/agent/.ssh/environment

chown agent:agent /home/agent/.ssh/environment
chmod 600 /home/agent/.ssh/environment

# $MODEL and $PROMPT (both optional) select the model and seed an initial prompt.
# The ${VAR:+...} and \" are kept literal here so they pass through the unquoted
# layout heredoc and KDL parsing untouched, then expand in the pane's bash from
# the SSH environment (both are propagated above). When a var is unset, its part
# of the command vanishes, leaving the agent's own default.
case "${AGENT:-claude-code}" in
  shell)
    AGENT_CMD='bash'
    ;;
  codex)
    # codex (a crossterm TUI) goes unresponsive after a zellij detach/reattach;
    # the Node agents don't. Wrapping it in its own tmux session gives codex a
    # stable pty that survives the reattach, so it keeps responding. zellij
    # still owns the SSH session — this tmux is just a per-codex shim.
    AGENT_CMD='tmux new-session -A -s codex codex ${MODEL:+-m \"$MODEL\"} ${PROMPT:+\"$PROMPT\"}'
    ;;
  gemini)
    AGENT_CMD='gemini ${MODEL:+-m \"$MODEL\"} ${PROMPT:+-i \"$PROMPT\"}'
    ;;
  copilot)
    AGENT_CMD='copilot ${MODEL:+--model \"$MODEL\"} ${PROMPT:+-p \"$PROMPT\"}'
    ;;
  *)
    AGENT_CMD='claude ${MODEL:+--model \"$MODEL\"} ${PROMPT:+\"$PROMPT\"}'
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

# Global stdout pipe: a world-writable FIFO any process can write to.
mkfifo /run/stdout
chmod 666 /run/stdout
exec 4<>/run/stdout
cat <&4 &

# The zellij session is created on the first SSH connection (ForceCommand is
# `zellij attach --create claude`) and kept alive across disconnects by
# `on_force_close "detach"` in config.kdl.
exec /usr/sbin/sshd -D
