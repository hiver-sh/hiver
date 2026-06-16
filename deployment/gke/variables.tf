variable "project_id" {
  description = "GCP project ID."
  type        = string
  default     = "agentic-468204"
}

variable "region" {
  description = "GCP region for the cluster control plane and regional resources."
  type        = string
  default     = "us-central1"
}

variable "zone" {
  description = "GCP zone for the (zonal) cluster and its nodes."
  type        = string
  default     = "us-central1-a"
}

variable "cluster_name" {
  description = "Name of the GKE cluster."
  type        = string
  default     = "sandbox"
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
  default     = "n1-standard-4"
}

variable "min_cpu_platform" {
  description = "Minimum CPU platform. Required for stable nested virtualization."
  type        = string
  default     = "Intel Haswell"
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
