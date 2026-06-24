# Design: Gateway Image Routing

Status: Proposal
Author: Emmanuel Garcia

## 1. Overview

The Envoy gateway handles two distinct routing concerns:

1. **Create** — client specifies an image; gateway routes to the right image's pod pool to create a sandbox instance
2. **Execute** — client targets a specific sandbox; gateway routes directly to the pod that owns it

These two legs are independent and compose cleanly.

The gateway is not the only path. Clients that have direct network access to the
image Services can bypass it entirely. The client library supports registering
a DNS name per image so callers can point an image at any reachable endpoint —
a local cluster, a private LoadBalancer, or the public gateway — without changing
call sites.

## 2. URL Structure

```
POST /v1/sandboxes/{key}                →  create a new sandbox (round-robin)
*    /sandbox/<id>/v1/<key>/...         →  talk to an existing sandbox (direct)
```

The `<id>` in the sandbox URL is a UUID whose first 4 bytes are the pod's IPv4
address. This encodes the routing target directly in the URL — no external state
or lookup needed.

The controller's `PUT /v1/sandboxes/{key}` becomes `POST /v1/sandboxes/{key}`.

## 3. Create Flow

The client populates an `x-hiver-image` header with the image name on every
create request. The gateway routes on this header — no URL parsing, no JSON body
inspection needed. It strips `/sandboxes` from the path so the pod's sandbox
server receives the request on its native `POST /v1/{key}` endpoint.

```
client
  │  POST /v1/sandboxes/{key}
  │  x-hiver-image: playwright
  ▼
Envoy gateway
  │  matches header x-hiver-image: playwright
  │  rewrites path: /v1/sandboxes/{key} → /v1/{key}
  │  routes to playwright cluster (STRICT_DNS → playwright Service)
  ▼
playwright Service  (ClusterIP, round-robin)
  │
  ▼
pod  POST /v1/{key}  (sandbox_server.yaml)
  │  picks a slot, creates microVM
  │  returns { id: "<uuid>", ... }   id encodes this pod's IP
  ▼
client stores id
```

The Envoy route match for this:

```yaml
- match:
    prefix: "/v1/sandboxes/"
    headers:
      - name: "x-hiver-image"
        string_match:
          exact: "playwright"
  route:
    cluster: playwright
    regex_rewrite:
      pattern:
        regex: "^/v1/sandboxes/"
      substitution: "/v1/"
    timeout: 0s
```

The pod constructs the `id` UUID by encoding its own IP (from the downward API
or `hostname -I`) into the first 4 bytes. Subsequent requests can reach the pod
directly without any registry.

## 4. Execute Flow

```
client
  │  GET /sandbox/<id>/v1/<key>/exec        (apiServerUrl = gateway/sandbox/<id>)
  ▼
Envoy gateway (Lua filter)
  │  extracts <id> from path: ^/sandbox/([^/?#]+)
  │  decodes first 4 UUID bytes → pod IPv4
  │  rewrites :authority to <pod-ip>:8099
  │  regex strips /sandbox/<id> → pod sees /v1/<key>/exec
  ▼
dynamic_forward_proxy cluster
  │  dials pod IP directly (bypasses kube-proxy and kube-dns)
  ▼
pod  receives  GET /v1/<key>/exec
```

Bypassing kube-dns avoids the ~10s negative-cache window on freshly created
sandbox DNS records.

## 5. Kubernetes Resources

Each image needs a **Deployment** and a **Service**:

```yaml
# one per image, e.g. playwright, chromium, ...
apiVersion: apps/v1
kind: Deployment
metadata:
  name: playwright
  namespace: hiver-sandbox
spec:
  replicas: N
  template:
    spec:
      containers:
        - name: sandbox
          image: hiversh/playwright:<tag>
          args: ["--pack", "--prewarm", ...]
          ports:
            - containerPort: 8099
---
apiVersion: v1
kind: Service
metadata:
  name: playwright
  namespace: hiver-sandbox
spec:
  selector:
    app: playwright
  ports:
    - port: 8099
      targetPort: 8099
```

No headless service is needed for the create path — kube-proxy round-robin is
fine here since we don't need pod-level affinity until *after* the pod returns
its UUID.

In the Kubernetes environment, pods are never created manually at runtime — the
Deployment controller manages the pod pool. Sandbox slots are claimed from
already-running pods via `POST /v1/{key}`. Manual pod creation (e.g. via the
controller's `kubectl run` path) is only supported in the Docker environment.

## 6. Image Catalog

Logical image names map to Docker image references. Two variants exist: one for
container isolation and one for microVM isolation. Kubernetes defaults to microVM.

| Name       | Container                          | microVM                                  |
|------------|------------------------------------|------------------------------------------|
| `claude`   | `hiversh/claude:latest`            | `hiversh/claude:latest-microvm`          |
| `codex`    | `hiversh/codex:latest`             | `hiversh/codex:latest-microvm`           |
| `copilot`  | `hiversh/copilot:latest`           | `hiversh/copilot:latest-microvm`         |
| `opencode` | `hiversh/opencode:latest`          | `hiversh/opencode:latest-microvm`        |
| `node`     | `hiversh/node:alpine`              | `hiversh/node:alpine-microvm`            |
| `python`   | `hiversh/python:3.13-alpine`       | `hiversh/python:3.13-alpine-microvm`     |
| `browser`  | `hiversh/chromium:latest`          | `hiversh/chromium:latest-microvm`        |

### Kubernetes Deployments

All images default to `replicas: N` except `browser` which runs a single pod
(`replicas: 1`) given its heavier resource footprint:

```yaml
# all images except browser
replicas: N   # tuned per workload

# browser
replicas: 1
```

The microVM image refs are used in all Kubernetes Deployments.

## 7. Controller Pod Discovery

The controller never creates pods in the Kubernetes environment. Instead it
discovers them through periodic polling and tracks their sandboxes via SSE:

1. **Poll** — every 5s, the controller lists pods in the `hiver-sandbox`
   namespace (by label selector). Any newly seen pod is registered; any pod that
   disappears is handled as described below.

2. **SSE connection** — for each live pod the controller opens a persistent
   connection to `GET /v1/events` on that pod. This stream carries `PodEvent`
   payloads for every inner-sandbox lifecycle transition (`starting`, `running`,
   `stopping`, `stopped`). The controller uses these to maintain its own sandbox
   state and surface them on the controller's event stream (`GET
   /v1/sandboxes/events`).

3. **Reconnect on failure** — if the SSE connection drops, the controller
   re-attempts it. The `lastEventId` query parameter is used to resume the
   stream without replaying already-processed events.

4. **Pod gone** — if a pod is no longer reachable (connection refused, pod
   removed from the list), all sandboxes believed to be running on that pod are
   marked dead. There is no attempt to recover them — the pod's memory is gone.

All state transitions collected from pod SSE streams are re-emitted on the
controller's own `GET /v1/sandboxes/events` stream, making them visible to
clients such as the inspector.

## 8. Envoy Config Changes

Add a STRICT_DNS cluster per image and a route that matches before `/sandbox/`:

```yaml
routes:
  - match:
      prefix: "/v1/sandboxes/"
      headers:
        - name: "x-hiver-image"
          string_match:
            exact: "playwright"
    route:
      cluster: playwright
      regex_rewrite:
        pattern:
          regex: "^/v1/sandboxes/"
        substitution: "/v1/"
      timeout: 0s

  - match:
      prefix: "/v1/sandboxes/"
      headers:
        - name: "x-hiver-image"
          string_match:
            exact: "chromium"
    route:
      cluster: chromium
      regex_rewrite:
        pattern:
          regex: "^/v1/sandboxes/"
        substitution: "/v1/"
      timeout: 0s

  - match:
      prefix: "/sandbox/"
    route:
      cluster: dynamic_forward_proxy
      regex_rewrite:
        pattern:
          regex: "^/sandbox/[^/]+"
        substitution: ""
      timeout: 0s

clusters:
  - name: playwright
    type: STRICT_DNS
    connect_timeout: 30s
    load_assignment:
      cluster_name: playwright
      endpoints:
        - lb_endpoints:
            - endpoint:
                address:
                  socket_address:
                    address: playwright.hiver-sandbox.svc.cluster.local
                    port_value: 8099

  - name: chromium
    type: STRICT_DNS
    connect_timeout: 30s
    load_assignment:
      cluster_name: chromium
      endpoints:
        - lb_endpoints:
            - endpoint:
                address:
                  socket_address:
                    address: chromium.hiver-sandbox.svc.cluster.local
                    port_value: 8099
```

## 9. Client-Side Image Registration

Clients that can reach an image's Service directly skip the gateway entirely.
The client library (`@hiver.sh/client`) exposes an optional registration API to
map image names to endpoints. Without it, all creates go through the gateway:

```typescript
import * as hiver from "@hiver.sh/client";

// point "playwright" at a private LB instead of going through the gateway
hiver.registerImage("playwright", "https://playwright.internal.example.com")

// or a local cluster during development
hiver.registerImage("playwright", "http://localhost:8099")
```

When creating a sandbox, the client checks its registry first:

```
image name → registered URL?
  yes → POST <registered-url>/v1/sandboxes/{key}  (x-hiver-image header still sent)
  no  → POST gateway/v1/sandboxes/{key}            (x-hiver-image header routes it)
```

The sandbox URL returned by the pod (`/sandbox/<id>/...`) is self-contained —
the `<id>` encodes the pod IP, so execute requests always route correctly
regardless of which path was used to create the sandbox.

This means:
- **Private deployments** can expose image Services on internal LoadBalancers
  and register them in the client — the gateway is never in the path
- **Mixed environments** work naturally: some images go direct, others through
  the gateway
- The gateway remains the fallback for clients that cannot reach Services directly
  (e.g. browser-based clients, external CI)

## 10. Reaching an Image Service from a Local Machine

Four options, in order of simplicity:

### Port-forward (local dev)

No external IP or DNS required:

```bash
kubectl port-forward svc/playwright 8099:8099 -n hiver-sandbox
```

```typescript
import * as hiver from "@hiver.sh/client";
hiver.registerImage("playwright", "http://localhost:8099")
```

### External IP directly

```bash
kubectl get svc playwright -n hiver-sandbox
# EXTERNAL-IP: 34.102.x.x
```

```typescript
import * as hiver from "@hiver.sh/client";
hiver.registerImage("playwright", "http://34.102.x.x:8099")
```

Works but the IP can change if the Service is recreated.

### `/etc/hosts` for a stable local name

```
# /etc/hosts
34.102.x.x  playwright.sandbox.local
```

```typescript
import * as hiver from "@hiver.sh/client";
hiver.registerImage("playwright", "http://playwright.sandbox.local:8099")
```

Per-machine and manual to update when the IP changes.

### Cloud DNS (shared / CI environments)

Register the external IP with a stable name in Cloud DNS (or Route 53, etc.):

```
playwright.sandbox.yourdomain.com  →  34.102.x.x
```

Resolves from anywhere — local machines, CI, other clusters — without per-machine
configuration. The right choice once beyond local dev.

## 11. Local Docker Environment (`hiver up`)

In the Docker environment, image-to-source mapping is configured via a JSON
config file rather than Kubernetes Deployments.

### Config file

The default location is `~/.hiver/config.json`, written by the hiver CLI. It
maps logical image names to configuration objects. `pack` controls whether the
pod runs with snapshot/prewarm enabled and defaults to `true`:

```json
{
  "images": {
    "playwright": {
      "ref":  "hiversh/playwright:microvm-39",
      "pack": true
    },
    "chromium": {
      "ref":  "hiversh/chromium:latest",
      "pack": true
    }
  }
}
```

### `hiver up`

```
hiver up [--config <path>]
```

`--config` overrides the default `~/.hiver/config.json`. `hiver up` passes the
config file path to Docker Compose, which mounts or injects it as an environment
variable read by the controller container. The controller (Docker runtime) then
handles spinning up the right image on demand.

### Explicit image names in Docker

In the Docker runtime, the client may also pass the full Docker image reference
directly instead of a logical name:

```typescript
await hiver.getOrCreateSandbox("my-sandbox", {
  image: "hiversh/playwright:microvm-39",   // full reference, bypasses mapping
});
```

This is useful during development or when working with custom/private images not
listed in the config file. The Kubernetes runtime always requires a registered
image name.

## 12. Upgrades

Rolling out a new image version is a standard Deployment rollout:

```
kubectl set image deployment/playwright sandbox=hiversh/playwright:<new-tag>
```

- Old pods continue serving their in-flight sandboxes (existing `<id>` URLs
  keep working — the UUID encodes the old pod's IP and Envoy routes there
  directly until the pod is gone)
- New pods start receiving `POST /v1/sandboxes/{key}` creates immediately
  once they pass readiness
- No sticky routing reconfiguration, no session migration

The only requirement is `terminationGracePeriodSeconds` long enough for pods to
finish any in-progress creates before terminating.
