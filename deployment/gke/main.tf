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

  // Guards against accidental `terraform destroy`. Set false, apply, then
  // destroy when intentionally tearing the cluster down.
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

  // Recycle nodes IN PLACE rather than adding a surge node first. GKE's default
  // (max_surge = 1) creates the replacement before draining the old node, which
  // needs a spare instance — and node_reservation holds exactly the nodes this
  // pool runs, so a surge upgrade fails with "reservation does not have
  // available resources" and the config change never lands. Trading surge for
  // max_unavailable keeps the upgrade inside the reservation's capacity, at the
  // cost of downtime while a node is drained and replaced (a single-node pool
  // goes fully unavailable).
  upgrade_settings {
    strategy        = "SURGE"
    max_surge       = var.node_upgrade_max_surge
    max_unavailable = var.node_upgrade_max_unavailable
  }

  node_config {
    machine_type     = var.machine_type
    min_cpu_platform = var.min_cpu_platform
    disk_size_gb     = var.disk_size_gb
    disk_type        = var.disk_type

    // Consume a specific reservation that holds n2 + Local SSD capacity in this
    // zone. Local SSD was exhausted on-demand across us-west1 and us-west2; the
    // reservation (deployment probe / `gcloud compute reservations create`)
    // guarantees the node schedules. The reservation's hardware (n2-standard-4,
    // 1x NVMe Local SSD, Cascade Lake) must match this node_config to be consumed.
    dynamic "reservation_affinity" {
      for_each = var.node_reservation != "" ? [1] : []
      content {
        consume_reservation_type = "SPECIFIC_RESERVATION"
        key                      = "compute.googleapis.com/reservation-name"
        values                   = [var.node_reservation]
      }
    }

    // GKE-managed ephemeral storage on Local SSD (NVMe). GKE formats the disks
    // and backs node ephemeral storage with them: emptyDir volumes and the
    // container writable/image layers all land on NVMe with no app changes.
    // This keeps the Firecracker prewarm fast path (snapshot/mem at
    // /run/firecracker, the container writable layer) on fast local flash, and
    // the sandbox snapshot dir (/snapshots, a pod-local emptyDir) on NVMe too —
    // ephemeral, not durable across pods. Each disk is a fixed 375 GiB (GCP
    // constant); total = count x 375 GiB. Local SSD is ephemeral (lost on node
    // stop/repair/upgrade) and only attachable at node creation, so changing the
    // count recreates the node pool.
    dynamic "ephemeral_storage_local_ssd_config" {
      for_each = var.local_nvme_ssd_count > 0 ? [1] : []
      content {
        local_ssd_count = var.local_nvme_ssd_count
      }
    }

    // Preallocate 2MiB hugepages at node boot so a sandbox pool can back guest
    // memory with hugetlbfs (chart: sandboxServices.<pool>.hugePages). Boot time
    // is the only workable moment: kubelet enumerates hugepages at startup, so
    // pages added later are invisible as node capacity and unrequestable by pods
    // — which is why this lives here and not in a sysctl or DaemonSet.
    // The memory is carved out permanently and is not reclaimable for normal
    // allocations, so 0 (the default) leaves nodes untouched.
    dynamic "linux_node_config" {
      for_each = var.hugepages_2m_count > 0 ? [1] : []
      content {
        hugepages_config {
          hugepage_size_2m = var.hugepages_2m_count
        }
      }
    }

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
