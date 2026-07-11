# MCP server — TypeScript

Bundle and run an MCP server inside the sandbox, then point the MCP Inspector at its proxied port.

The agent image in [`image/`](image/) is bundled automatically with `hiver bundle` on first run.

Start the gateway first with `hiver up`, then:

```bash
npm install
npm run build
npm start
```
