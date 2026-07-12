# Hiver control-plane Helm chart

Deploys the Hiver control plane (controller + Envoy gateway) and the per-image
sandbox pools. The gateway's per-image Envoy routes/clusters and the per-image
Deployment+Service are both generated from a single `images` list in
[values.yaml](values.yaml), so adding an image is one entry.

```sh
helm upgrade --install hiver .   # values in values.yaml
```

Released versions are also published to the Hiver chart repository (indexed on
[Artifact Hub](https://artifacthub.io/packages/helm/hiver/hiver)) under the same
version as the CLI. This is the recommended way to install a released version:

```sh
helm repo add hiver https://hiver-sh.github.io/hiver
helm repo update
helm install hiver hiver/hiver                       # latest published version
helm search repo hiver/hiver --versions             # list available versions
helm install hiver hiver/hiver --version <x.y.z>    # pin a specific version
```

The chart creates its own `hiver` and `hiver-sandbox` namespaces, so no
`--namespace`/`--create-namespace` flags are needed. Every image ships pinned to
an immutable digest, so a given chart version always installs the exact images it
was released with.

## Prerequisites

Install the Helm CLI (v3) and point `kubectl` at the target cluster.

```sh
# macOS
brew install helm

# Linux (official installer)
curl -fsSL https://raw.githubusercontent.com/helm/helm/main/scripts/get-helm-3 | bash
```

Other platforms and package managers are covered in the
[Helm install docs](https://helm.sh/docs/intro/install/). Verify with:

```sh
helm version
```

## Install

From this directory, install (or upgrade) the release. The chart creates its own
`hiver` and `hiver-sandbox` namespaces, so no `--create-namespace` is needed.

```sh
cd deployment/k8s/chart
helm upgrade --install hiver .
```

It deploys:

- **controller** — runs the Kubernetes runtime (`HIVE_RUNTIME=k8s`), provisioning
  each sandbox as a ConfigMap + privileged Pod + Service.
- **gateway** — envoy front door (LoadBalancer `:80` → `:10000`) routing
  `/controller/*` to the controller and `/sandbox/<id>/*` to per-sandbox Services.
- **per-image pools** — one Deployment+Service per logical image.

## Adding an image

Append one entry to `images:` in [values.yaml](values.yaml). The chart generates
both the Deployment+Service **and** the matching gateway Envoy route+cluster from
that single list, so they can't drift.

## Gateway public IP

The gateway is a `LoadBalancer` Service, so the cloud assigns it an external IP
(takes a minute after install). Fetch it with:

```sh
kubectl get svc gateway -n hiver -o jsonpath='{.status.loadBalancer.ingress[0].ip}'
```

Then reach the API at that IP on port 80:

```sh
GW=$(kubectl get svc gateway -n hiver -o jsonpath='{.status.loadBalancer.ingress[0].ip}')
curl "http://$GW/controller/..."     # controller API
curl "http://$GW/sandbox/<id>/..."   # a specific sandbox
```

A `<pending>` external IP just means the load balancer is still provisioning.

## Connecting the CLI

Point the `hiver` CLI at the deployed gateway with `hiver connect`:

```sh
GW=$(kubectl get svc gateway -n hiver -o jsonpath='{.status.loadBalancer.ingress[0].ip}')
hiver connect "http://$GW"   # gateway listens on port 80

hiver list                   # now hits the deployed gateway
hiver start --image claude
```

Because the gateway is remote, the CLI does **not** pull or build images
locally — the in-cluster controller resolves and pulls them itself. To switch
back to a local stack, run `hiver up` (or `hiver connect http://localhost:10000`).

### Pointing the client at the gateway

The TypeScript, Python, and Go clients read the gateway from the
`HIVER_GATEWAY_URL` environment variable, so the same address works without
changing code. An explicit URL passed in code still wins; otherwise the env var
is used, falling back to `http://localhost:10000`.

```sh
export HIVER_GATEWAY_URL="http://$GW"
```

```ts
// TypeScript — no gatewayUrl needed; picks up HIVER_GATEWAY_URL
await getOrCreateSandbox("my-key", { image: "claude" });
```

```py
# Python
await get_or_create_sandbox("my-key", SandboxConfig(image="claude"))
```

```go
// Go — empty string falls back to HIVER_GATEWAY_URL, then the default
import hiver "github.com/hiver-sh/hiver/client"

c := hiver.NewClient("")
```

## Namespaces

| Namespace       | Contents            | Pod Security |
| --------------- | ------------------- | ------------ |
| `hiver`         | controller, gateway | `baseline`   |
| `hiver-sandbox` | sandbox Pods        | `privileged` |

Sandboxes run in a separate `privileged` namespace so privileged execution is
confined to sandboxes, not the control plane. The controller's ServiceAccount
lives in `hiver` but is granted a Role in `hiver-sandbox` via a cross-namespace
RoleBinding. `baseline` (not `restricted`) is used for `hiver` because the
controller and gateway images run as root.
