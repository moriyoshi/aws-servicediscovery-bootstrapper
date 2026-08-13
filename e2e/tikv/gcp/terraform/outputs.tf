output "project" {
  value = var.project
}

output "region" {
  value = var.region
}

output "name_prefix" {
  value = local.prefix
}

output "pd_pool" {
  description = "Worker pool backing PD."
  value       = google_cloud_run_v2_worker_pool.pd.name
}

output "tikv_pool" {
  description = "Worker pool backing the TiKV stores."
  value       = google_cloud_run_v2_worker_pool.tikv.name
}

output "pd_instance_count" {
  value = var.pd_instance_count
}

output "tikv_instance_count" {
  value = var.tikv_instance_count
}

output "namespace_name" {
  description = "Service Directory namespace muster's instances() resolves against."
  value       = google_service_directory_namespace.tikv.namespace_id
}

output "pd_discovery_name" {
  description = "Service Directory service PD registers itself under."
  value       = local.pd_discovery_name
}

output "tikv_discovery_name" {
  description = "Service Directory service TiKV registers itself under."
  value       = local.tikv_discovery_name
}

output "pd_client_port" {
  value = local.pd_client_port
}

output "kv_bucket" {
  value = google_storage_bucket.kv.name
}

# Informational. The Makefile derives the same values from the project, region
# and prefix rather than reading these back, because `make bootstrap` is a
# targeted apply and Terraform does not necessarily evaluate outputs during one.
output "docker_registry" {
  description = "Registry host the node images live under."
  value       = local.registry_host
}

output "image_repo" {
  value = local.image_repo
}

output "network" {
  value = google_compute_network.main.name
}
