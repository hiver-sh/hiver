# Egress events — TypeScript

Subscribe to the event stream, make one allowed and one blocked outbound request from inside the sandbox, and print the `egress.request` / `egress.response` events the proxy emits.

Start the gateway first with `hiver up`, then:

```bash
npm install
npm start
```
