# Hiver Helm chart

Deploys the Hiver control plane (controller + Envoy gateway) and the per-service
sandbox pools.

## Prerequisites

Install the Helm CLI (v3) and point `kubectl` at the target cluster.

```sh
# macOS
brew install helm

# Linux (official installer)
curl -fsSL https://raw.githubusercontent.com/helm/helm/main/scripts/get-helm-3 | bash
```

Verify with:

```sh
helm version
```

## Add the chart

Released versions are published to the Hiver chart repository (indexed on
[Artifact Hub](https://artifacthub.io/packages/helm/hiver/hiver)) under the same
version as the CLI. Install from the repository:

```sh
helm repo add hiver https://hiver-sh.github.io/hiver
helm repo update
helm install hiver hiver/hiver                       # latest published version
helm search repo hiver/hiver --versions              # list available versions
```

The chart creates its own `hiver` and `hiver-sandbox` namespaces. Every image ships pinned to
an immutable digest, so a given chart version always installs the exact images it
was released with.

## Install

Install (or upgrade) the release. The chart creates its own `hiver` and
`hiver-sandbox` namespaces, so no `--create-namespace` is needed.

```sh
helm upgrade --install hiver hiver/hiver
```

It deploys:

- **controller** — runs the Kubernetes runtime (`HIVE_RUNTIME=k8s`), provisioning
  each sandbox as a ConfigMap + privileged Pod + Service.
- **gateway** — envoy front door (LoadBalancer `:80` → `:10000`) routing
  `/controller/*` to the controller and `/sandbox/<id>/*` to per-sandbox Services.
- **per-image pools** — one Deployment+Service per logical image.

## Configuration

See every setting and its default with:

```sh
helm show values hiver/hiver
```

Override selected keys with your own values file and/or `--set`. Both work on
`install` and `upgrade`, and only the keys you specify change.

The sandbox pools live under `sandboxServices:`, keyed by service name (a
**map**, not a list), so you can override a single field of a single pool
without redeclaring the others — e.g. changing the number of replicas for
claude:

```sh
# my-values.yaml — just the keys you want to change
cat > my-values.yaml <<'EOF'
sandboxServices:
  claude:
    replicas: 3
EOF

helm upgrade --install hiver hiver/hiver -f my-values.yaml
# or inline:
helm upgrade --install hiver hiver/hiver --set sandboxServices.claude.replicas=3
```

Note: Helm does **not** remember overrides from a previous install unless you
also pass `--reuse-values`.

### Isolation (microvm vs container)

Each service ships both a **microvm** and a **container** image variant and
picks one with its own `isolation` field (default `microvm`). Switch a single
service to the container variant:

```sh
helm upgrade --install hiver hiver/hiver --set sandboxServices.claude.isolation=container
```

`microvm` runs each sandbox in a VM (stronger isolation); `container` runs it as
an ordinary container (lighter, shared kernel). To move every pool, set
`isolation` on each — there is no global switch.

> **Genuine lists are still replaced, not merged.** Helm deep-merges maps but
> overwrites lists wholesale, so to change a list value (e.g. `controller.env`)
> you must supply the entire list. `sandboxServices:` is a map, so it's exempt —
> see [Adding a service](#adding-a-service).

### Memory backend

Control how a **resumed** microVM
gets its guest memory back. They matter only for pools that resume from a
vm snapshot.

- **`memBackend: uffd`** — serve guest memory from a userfaultfd handler that
  populates it in the background, instead of letting firecracker map
  `mem.bin` and demand-page it in 4KiB at a time. This is the knob to reach
  for first: measured on the claude pool it trades ~15ms of extra snapshot-load
  time for ~725ms less first-token latency (~15,545 faults / ~740ms down to 15
  faults / ~125ms). Costs residency — the guest becomes fully resident on
  resume (~514MiB vs ~271MiB measured here) instead of paging in its working
  set, so budget the whole guest per concurrent VM, not just its working set.
- **`hugePages: "2M"`** (or `"1G"`) — back guest memory with hugetlbfs pages
  instead of 4KiB ones, shrinking the same working set to ~48 faults at 2MiB
  granularity. **Implies `memBackend: uffd` on its own** — firecracker cannot
  map a hugetlbfs-backed snapshot through the plain `File` backend — so don't
  set `memBackend` alongside it unless you like redundant config.
- **`hugePagesLimit`** — overrides the size of the pod's `hugepages-2Mi` (or
  `-1Gi`) resource limit, which the chart otherwise derives from
  `resources.requests.memory` (one guest's worth). Hugepage memory is a hard,
  non-reclaimable allocation, separate from `resources.limits.memory`, and a
  pool pod runs **several microVMs concurrently** — so the derived default
  under-sizes any pool with concurrency > 1. Set it explicitly to cover peak
  concurrent VMs per pod, or firecracker fails later VM boots once the
  allocation is exhausted (no fallback to 4KiB pages).

```yaml
sandboxServices:
  claude:
    hugePages: "2M"      # implies memBackend: uffd
    hugePagesLimit: 2Gi  # e.g. 4 concurrent VMs x 512Mi guest each
```

**Before enabling `hugePages`, two things have to be true or the guest fails
to boot:**

1. **The node has a preallocated hugetlb pool.** The chart only sets the
   container's resource *limit* — it does not provision the node, and a guest
   fails to boot with ENOMEM until this exists separately. On GKE, set the
   [`hugepages_2m_count`](../gke/variables.tf) Terraform variable on the node
   pool. This must happen at **node boot**: kubelet enumerates hugepages at
   startup, so a pool added afterward shows up in `/proc/meminfo` but stays 0
   in `kubectl get node -o jsonpath='{.status.capacity}'`, and pods can never
   request it.
2. **Any existing snapshot is re-captured.** `hugePages` is boot-time guest
   state baked into the VM snapshot at capture time, so an existing base
   snapshot must be re-captured before a resume sees any benefit (or boots
   correctly at all).

**When to use which:**

- Default (neither set) — fine for pools that mostly cold-boot, or where
  first-token latency on resume doesn't matter.
- `memBackend: uffd` alone — the common case for a warmed, frequently-resumed
  pool: faster resume without a node-level hugepage carve-out to manage.
- `hugePages` — squeeze out the remaining fault overhead on high-concurrency
  resume-heavy pools, in exchange for permanently reserving node memory that
  can't be reclaimed for anything else, and remembering to keep
  `hugePagesLimit` sized as concurrency changes.

## Upgrading

`helm upgrade --install` is idempotent so the same command installs a fresh
release or upgrades an existing one to a new chart version:

```sh
helm upgrade --install hiver hiver/hiver --version <x.y.z>
```

Use `helm upgrade` (not `helm install`) once a release named `hiver` already
exists; `helm install` refuses a name that is still in use.

## Adding a service

A "service" is a pool of prewarmed pods for one image. Deploying a new one is
two steps: **build the image as a Hiver bundle**, then **add an entry under
`sandboxServices:`**. The chart does the rest:
generates the Deployment, the headless Service, and the matching gateway Envoy
route+cluster, so they can't drift. No template edits, no gateway config.

### 1. Build the image as a Hiver bundle

A pool image can't be an arbitrary container — it must be a **Hiver bundle**: an
image prepared so its pods can serve sandboxes. Turn a Dockerfile directory (or
an existing image ref) into one with `hiver bundle`:

```sh
# container variant
hiver bundle ./docker/mytool --entrypoint="tail -f /dev/null" \
  --tag myrepo/mytool:1.0.0 --push --platform linux/amd64,linux/arm64

# microvm variant (add --microvm)
hiver bundle ./docker/mytool --entrypoint="tail -f /dev/null" --microvm \
  --tag myrepo/mytool:1.0.0-microvm --push --platform linux/amd64,linux/arm64
```

- `--entrypoint` sets the command each sandbox starts with — use a long-running
  one like `tail -f /dev/null` for a general-purpose pool you exec into.
- `--microvm` builds the microvm variant; omit it for the container variant.
  Build only the variant(s) your pool's `isolation` uses.
- `--tag` names the result; `--push --platform ...` publishes it as a multi-arch
  image to a registry the cluster can pull from.

Pointing a pool at a plain image that isn't a bundle deploys pods that never
serve sandboxes.

### 2. Add it to `sandboxServices`

Add a new key under `sandboxServices:` in your values file — it deep-merges in,
so the built-in pools are untouched. `image` may be a plain string (single
variant, `isolation` ignored) or a map keyed by isolation mode:

```yaml
# my-values.yaml
sandboxServices:
  mytool:
    image:
      microvm: myrepo/mytool:1.0.0-microvm
      container: myrepo/mytool:1.0.0
    isolation: microvm
    replicas: 1
    maxConcurrentLaunches: 4
    resources:
      requests:
        cpu: "1"
        memory: 512Mi
      limits:
        cpu: "4"
        memory: 4Gi
```

```sh
helm upgrade --install hiver hiver/hiver -f my-values.yaml
```

Each entry needs at least an `image` and `replicas`; see `helm show
values hiver/hiver` for the full shape (`isolation`, `maxConcurrentLaunches`,
`resources`, `upstreamPoolScope`, etc.). A `replicas: 0` pool deploys the route
and Service but keeps no warm pods until scaled up.

### 3. Start sandbox

```sh
hiver start --image mytool           # routes to the mytool service
```

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
