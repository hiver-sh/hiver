// Zonal GKE cluster with a dedicated, separately-managed node pool.
// The default node pool is removed so the node spec lives entirely in
// google_container_node_pool below (recommended GKE pattern).

resource "google_container_cluster" "this" {
  name     = var.cluster_name
  location = var.zone

  remove_default_node_pool = true
  initial_node_count       = 1

  release_channel {
    channel = var.release_channel
  }

  // Use VPC-native (alias IP) networking; let GKE pick the secondary ranges.
  networking_mode = "VPC_NATIVE"
  ip_allocation_policy {}

  // Avoid accidental deletion of a running cluster via terraform.
  deletion_protection = true
}

resource "google_container_node_pool" "primary" {
  name     = "${var.cluster_name}-pool"
  location = var.zone
  cluster  = google_container_cluster.this.name

  initial_node_count = var.node_count

  autoscaling {
    min_node_count = var.min_node_count
    max_node_count = var.max_node_count
  }

  management {
    auto_repair  = true
    auto_upgrade = true
  }

  node_config {
    machine_type     = var.machine_type
    min_cpu_platform = var.min_cpu_platform
    disk_size_gb     = var.disk_size_gb
    disk_type        = var.disk_type

    // Nested virtualization requires the Ubuntu node image (KVM is not
    // available on Container-Optimized OS).
    image_type = "UBUNTU_CONTAINERD"

    advanced_machine_features {
      enable_nested_virtualization = var.enable_nested_virtualization
      # Required by the google provider. 2 = keep default SMT (n1-standard-4 is
      # 2 physical cores x 2 threads); set to 1 to disable hyper-threading.
      threads_per_core = 2
    }

    // Mirror the source VM's Shielded VM posture: vTPM + integrity
    // monitoring on, Secure Boot off.
    shielded_instance_config {
      enable_secure_boot          = false
      enable_integrity_monitoring = true
    }

    oauth_scopes = [
      "https://www.googleapis.com/auth/cloud-platform",
    ]

    metadata = {
      disable-legacy-endpoints = "true"
    }

    labels = {
      "goog-ec-src" = "vm_add-gcloud"
    }
  }
}
