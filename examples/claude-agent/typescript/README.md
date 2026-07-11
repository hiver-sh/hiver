# Claude agent + MCP server — TypeScript

Hook the Claude Agent SDK up to a Hiver-managed MCP server bundled from `image/`, so every tool call the model makes is mediated by the sandbox's egress + FUSE policies. An interactive quant-trading agent backed by the Finnhub API.

Requires `ANTHROPIC_API_KEY`, `FINNHUB_API_KEY` in the environment.

The agent image in [`image/`](image/) is bundled automatically with `hiver bundle` on first run.

Start the gateway first with `hiver up`, then:

```bash
npm install
npm run build
export ANTHROPIC_API_KEY=...
export FINNHUB_API_KEY=...
npm start
```
