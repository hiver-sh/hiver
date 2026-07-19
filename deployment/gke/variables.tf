variable "project_id" {
  description = "GCP project ID."
  type        = string
  default     = "agentic-468204"
}

variable "region" {
  description = "GCP region for the cluster control plane and regional resources."
  type        = string
  default     = "us-west2"
}

variable "zone" {
  description = "GCP zone for the (zonal) cluster and its nodes. us-west2-a: us-west1 was fully out of n2 + Local SSD capacity; us-west2-a had it (confirmed via reservation) and is the next-closest region to SF."
  type        = string
  default     = "us-west2-a"
}

variable "node_reservation" {
  description = "Name of a specific Compute Engine reservation the node pool must consume (guarantees Local SSD capacity in the zone). Empty disables reservation affinity."
  type        = string
  default     = "hiver-pool-res"
}

variable "cluster_name" {
  description = "Name of the GKE cluster. Also derives the node pool name."
  type        = string
  default     = "hiver"
}

variable "node_count" {
  description = "Number of nodes per zone in the node pool. Ignored when autoscaling bounds are used below."
  type        = number
  default     = 1
}

variable "min_node_count" {
  description = "Minimum nodes per zone for autoscaling."
  type        = number
  default     = 1
}

variable "max_node_count" {
  description = "Maximum nodes per zone for autoscaling."
  type        = number
  default     = 3
}

# --- Node spec (derived from vm-config.json) ---

variable "machine_type" {
  description = "Compute Engine machine type for nodes."
  type        = string
  default     = "n2-standard-4"
}

variable "min_cpu_platform" {
  description = "Minimum CPU platform. Required for stable nested virtualization. n2's base is Cascade Lake (Haswell is invalid for n2)."
  type        = string
  default     = "Intel Cascade Lake"
}

variable "disk_size_gb" {
  description = "Boot disk size per node, in GB."
  type        = number
  default     = 50
}

variable "disk_type" {
  description = "Boot disk type."
  type        = string
  default     = "pd-balanced"
}

variable "local_nvme_ssd_count" {
  description = "Number of Local SSD (NVMe) disks per node, used as GKE-managed ephemeral storage (backs emptyDir + container layers). Each disk is a fixed 375 GiB (GCP constant). 0 disables. Changing this recreates the node pool."
  type        = number
  default     = 1
}

variable "hugepages_2m_count" {
  description = <<-EOT
    Number of 2MiB hugepages to preallocate per node (0 disables). Set this to
    let a sandbox pool run with the chart's `hugePages: \"2M\"`, which backs guest
    memory with hugetlbfs: a resumed microVM otherwise faults its working set in
    one 4KiB page at a time (measured ~15.5k faults / ~740ms on the first turn
    after a resume, vs ~2k / ~60ms cold-booted).

    It must be set HERE rather than by a sysctl or DaemonSet after the fact:
    kubelet enumerates hugepages at startup, so pages allocated later show up in
    /proc/meminfo but stay 0 in the node's reported capacity — and pods can then
    never request them.

    Sizing: total hugepage bytes = count x 2MiB, carved permanently out of node
    memory and NOT reclaimable for normal allocations. Cover every guest a node
    hosts concurrently; firecracker fails a boot when the pool is exhausted
    rather than falling back to 4KiB pages. e.g. 2048 = 4 GiB = 8 concurrent
    512MiB guests. Changing this recreates the node pool.
  EOT
  type        = number
  default     = 0
}

variable "enable_nested_virtualization" {
  description = "Enable nested virtualization on nodes (KVM inside the node)."
  type        = bool
  default     = true
}

variable "release_channel" {
  description = "GKE release channel: RAPID, REGULAR, STABLE, or UNSPECIFIED."
  type        = string
  default     = "REGULAR"
}
