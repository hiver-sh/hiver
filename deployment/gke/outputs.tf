output "cluster_name" {
  description = "Name of the GKE cluster."
  value       = google_container_cluster.this.name
}

output "cluster_endpoint" {
  description = "Control-plane endpoint."
  value       = google_container_cluster.this.endpoint
  sensitive   = true
}

output "cluster_location" {
  description = "Location (zone) of the cluster."
  value       = google_container_cluster.this.location
}

output "get_credentials_command" {
  description = "Command to configure kubectl for this cluster."
  value       = "gcloud container clusters get-credentials ${google_container_cluster.this.name} --zone ${google_container_cluster.this.location} --project ${var.project_id}"
}
