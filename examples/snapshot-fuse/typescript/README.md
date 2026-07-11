# Snapshot (FUSE / GCS) — TypeScript

Persist a snapshot through an internal GCS-backed FUSE drive instead of the host's local disk, so it survives even if the next boot lands on a different host.

Requires `GCS_BUCKET`, `GCS_SERVICE_ACCOUNT_JSON` in the environment.

Start the gateway first with `hiver up`, then:

```bash
npm install
export GCS_BUCKET=...
export GCS_SERVICE_ACCOUNT_JSON=...
npm start
```
