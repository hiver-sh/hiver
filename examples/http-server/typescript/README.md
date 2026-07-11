# HTTP server — TypeScript

Proxy HTTP requests from the host to services running inside the sandbox via `sandbox.proxyUrl(port)`. The image runs two echo servers on ports 8080 and 9000.

The agent image in [`image/`](image/) is bundled automatically with `hiver bundle` on first run.

Start the gateway first with `hiver up`, then:

```bash
npm install
npm run build
npm start
```
