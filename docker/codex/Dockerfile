FROM agent-base

RUN npm install -g @openai/codex

RUN mkdir -p /home/agent/.codex
COPY .codex/config.json /home/agent/.codex/config.json
RUN chown -R agent:agent /home/agent/.codex
