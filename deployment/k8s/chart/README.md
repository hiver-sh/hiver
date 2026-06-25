# Hiver control-plane Helm chart

Deploys the Hiver control plane (controller + Envoy gateway) and the per-image
sandbox pools. The gateway's per-image Envoy routes/clusters and the per-image
Deployment+Service are both generated from a single `images` list in
[values.yaml](values.yaml), so adding an image is one entry.

```sh
helm upgrade --install hiver .   # values in values.yaml
```

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

Override any value at install time with `--set` or a values file, e.g. to pin a
different controller image:

```sh
helm upgrade --install hiver . --set controller.image=hiversh/controller:v0.1.18
```

Check the rollout, then grab the gateway's public IP (see below):

```sh
kubectl rollout status deploy/controller -n hiver
kubectl rollout status deploy/gateway -n hiver
```

To tear the release down:

```sh
helm uninstall hiver
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
