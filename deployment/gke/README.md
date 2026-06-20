# GKE cluster (Terraform)

Provisions a zonal GKE cluster with a dedicated node pool whose node spec
mirrors the source VM in `vm-config.json` — most importantly **nested
virtualization** enabled so KVM works inside the nodes. Nodes are backed by
**Local SSD (NVMe)** ephemeral storage so the Firecracker prewarm fast path
(snapshot/mem under `/run/firecracker`, the container writable layer) lands on
fast, node-local flash.

The cluster runs in **`us-west1` (Oregon)** — the lowest-latency GCP region to
the SF Bay Area. Nodes use **`n2-standard-4`**: `n1` + Local SSD was repeatedly
out of capacity (`ZONE_RESOURCE_POOL_EXHAUSTED`) across zones, and the newer
`n2` family has much better Local SSD availability (and still supports nested
virtualization).

Keyed snapshots (`/snapshots`) currently land on that same node-local NVMe (a
pod-local `emptyDir`) — fast but ephemeral, not durable across pods. See
[Snapshots](#snapshots).

## Node spec

| Setting               | Value                             | Source                                               |
| --------------------- | --------------------------------- | ---------------------------------------------------- |
| Region / zone         | `us-west1` / `us-west1-b`         | `region` / `zone` (Oregon, closest to SF)            |
| Machine type          | `n2-standard-4`                   | `machine_type` (n2: better Local SSD availability)   |
| Min CPU platform      | `Intel Cascade Lake`              | `min_cpu_platform` (n2 base; supports nested virt)   |
| Boot disk             | 50 GB                             | `disks[0].diskSizeGb`                                |
| Local SSD (NVMe)      | 1 × 375 GiB, ephemeral            | `local_nvme_ssd_count`                               |
| Nested virtualization | enabled                           | `advancedMachineFeatures.enableNestedVirtualization` |
| Shielded VM           | vTPM + integrity, Secure Boot off | `shieldedInstanceConfig`                             |
| Node image            | `UBUNTU_CONTAINERD`               | required for nested virt (no KVM on COS)             |

## Prerequisites

- `terraform >= 1.5`, `gcloud` authenticated (`gcloud auth application-default login`)
- The `container.googleapis.com` and `compute.googleapis.com` APIs enabled on the project.

## Usage

```sh
cd deployment/gke
cp terraform.tfvars.example terraform.tfvars   # edit if needed
terraform init
terraform plan
terraform apply
```

Then point `kubectl` at the cluster:

```sh
$(terraform output -raw get_credentials_command)
```

## Workloads (`k8s/`)

The `k8s/` directory deploys the control plane with `kubectl apply -k k8s`:

- **controller** — runs the Kubernetes runtime (`HIVE_RUNTIME=k8s`), provisioning
  each sandbox as a ConfigMap + privileged Pod + Service.
- **gateway** — envoy front door (LoadBalancer `:80` → `:10000`) routing
  `/controller/*` to the controller and `/sandbox/<id>/*` to per-sandbox Services.

### Gateway public IP

The gateway is a `LoadBalancer` Service, so GCP assigns it an external IP (takes
a minute after `apply`). Fetch it with:

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

### Namespaces

| Namespace       | Contents            | Pod Security |
| --------------- | ------------------- | ------------ |
| `hiver`         | controller, gateway | `baseline`   |
| `hiver-sandbox` | sandbox Pods        | `privileged` |

Sandboxes run in a separate `privileged` namespace so privileged execution is
confined to sandboxes, not the control plane. The controller's ServiceAccount
lives in `hiver` but is granted a Role in `hiver-sandbox` via a cross-namespace
RoleBinding. `baseline` (not `restricted`) is used for `hiver` because the
controller and gateway images run as root.

### Gateway envoy config is duplicated

`k8s/gateway.yaml` ships a `gateway-envoy` ConfigMap that **mirrors
`docker/gateway/envoy.yaml`** with one change: the sandbox `:authority` is
rewritten to a fully-qualified cross-namespace name
(`hiver-sandbox-<id>.hiver-sandbox.svc.cluster.local:8099`) so the gateway in
`hiver` can reach sandbox Services in `hiver-sandbox`. **If the source
`docker/gateway/envoy.yaml` changes, update this ConfigMap to match.**

## Notes

- The cluster has `deletion_protection = true`; `terraform destroy` will fail
  until you set it to `false` and re-apply.
- Nested virtualization requires the Ubuntu node image and a CPU platform of
  Haswell or newer — both are set in `main.tf` / `variables.tf`.
- The node pool autoscales between `min_node_count` and `max_node_count` per zone.
- **Local SSD is NVMe and ephemeral.** GKE manages it as node ephemeral storage
  (`ephemeral_storage_local_ssd_config`), backing `emptyDir` and container
  layers — no app changes needed. Each disk is a fixed 375 GiB (GCP constant);
  `local_nvme_ssd_count` sets the disk count. Data is **wiped on node
  stop/repair/upgrade** (and `auto_repair`/`auto_upgrade` are on), so it is
  scratch only — durable, cross-pod snapshots still need an RWX PV (e.g.
  Filestore) mounted at `/snapshots`.
- **Local SSD only attaches at node creation**, so changing `local_nvme_ssd_count`
  recreates the node pool. The cluster is zonal, so it must run in a zone that
  has Local SSD capacity for the machine type; `us-central1-a` did not, which is
  why this moved to `us-west1-b`.
