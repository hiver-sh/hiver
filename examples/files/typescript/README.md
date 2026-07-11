# Files — TypeScript

Write a file into a sandbox mount from the host and read it back out. Both calls bypass the per-mount ACLs — the control plane is higher privilege than the agent.

Start the gateway first with `hiver up`, then:

```bash
npm install
npm start
```
