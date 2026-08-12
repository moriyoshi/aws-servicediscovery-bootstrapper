# An HTTP (API-only) CloudMap namespace: the tasks find each other with
# DiscoverInstances, which is exactly what muster's instances() builtin calls.
# ECS Service Connect does the registration and deregistration.
resource "aws_service_discovery_http_namespace" "tikv" {
  name        = "${local.prefix}tikv"
  description = "muster e2e: TiKV cluster membership"
  tags = {
    Name = "${local.prefix}tikv"
  }
}
