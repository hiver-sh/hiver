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
