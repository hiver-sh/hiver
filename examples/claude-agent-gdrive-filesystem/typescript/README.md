# Claude agent + Google Drive — TypeScript

The Claude-agent + MCP-server example, with generated files persisted to a Google Drive FUSE mount across runs.

Requires `ANTHROPIC_API_KEY`, `FINNHUB_API_KEY`, `GOOGLE_CLIENT_ID`, `GOOGLE_CLIENT_SECRET` in the environment.

The agent image in [`image/`](image/) is bundled automatically with `hiver bundle` on first run.

Start the gateway first with `hiver up`, then:

```bash
npm install
npm run build
export ANTHROPIC_API_KEY=...
export FINNHUB_API_KEY=...
export GOOGLE_CLIENT_ID=...
export GOOGLE_CLIENT_SECRET=...
npm start
```
