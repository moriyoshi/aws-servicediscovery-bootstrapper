output "region" {
  value = local.aws_region
}

output "name_prefix" {
  value = local.prefix
}

output "ecs_cluster" {
  value = aws_ecs_cluster.main.name
}

output "ecs_cluster_arn" {
  value = aws_ecs_cluster.main.arn
}

output "pd_service" {
  description = "ECS service name for PD."
  value       = aws_ecs_service.tikv_pd.name
}

output "tikv_service" {
  description = "ECS service name for TiKV."
  value       = aws_ecs_service.tikv_node.name
}

output "pd_desired_count" {
  value = var.pd_desired_count
}

output "tikv_desired_count" {
  value = var.tikv_desired_count
}

output "namespace_name" {
  value = aws_service_discovery_http_namespace.tikv.name
}

output "namespace_id" {
  value = aws_service_discovery_http_namespace.tikv.id
}

output "pd_discovery_name" {
  description = "CloudMap service name PD registers under."
  value       = local.pd_discovery_name
}

output "tikv_discovery_name" {
  description = "CloudMap service name TiKV registers under."
  value       = local.tikv_discovery_name
}

output "pd_client_port" {
  description = "Port PD serves its client API on, inside the task's network namespace."
  value       = 2379
}

output "kv_table" {
  value = aws_dynamodb_table.kv.name
}

output "ecr_registry" {
  description = "Registry host to push the task images to."
  value       = split("/", aws_ecr_repository.tikv_pd.repository_url)[0]
}

output "ecr_repository_tikv_pd" {
  value = aws_ecr_repository.tikv_pd.repository_url
}

output "ecr_repository_tikv_node" {
  value = aws_ecr_repository.tikv_node.repository_url
}

output "log_group_tikv_pd" {
  value = aws_cloudwatch_log_group.tikv_pd.name
}

output "log_group_tikv_node" {
  value = aws_cloudwatch_log_group.tikv_node.name
}
