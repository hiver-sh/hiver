# Internal service — TypeScript

Start with a deny-all egress policy, fail to reach an internal host, then `applyConfig` to allow it and succeed.

Start the gateway first with `hiver up`, then:

```bash
npm install
npm start
```
