# GKE cluster (Terraform)

Provisions a zonal GKE cluster with a dedicated node pool whose node spec
mirrors the source VM in `vm-config.json` — most importantly **nested
virtualization** enabled so KVM works inside the nodes.

## Node spec

| Setting               | Value                             | Source                                               |
| --------------------- | --------------------------------- | ---------------------------------------------------- |
| Machine type          | `n1-standard-4`                   | `machineType`                                        |
| Min CPU platform      | `Intel Haswell`                   | `minCpuPlatform`                                     |
| Boot disk             | 50 GB                             | `disks[0].diskSizeGb`                                |
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
