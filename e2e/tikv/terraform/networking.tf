# Nothing here is reachable from outside the VPC. There is no internet gateway,
# no NAT and no load balancer: the subnet's route table carries only the VPC's
# local route plus the two gateway endpoints, so a task cannot reach the
# internet and the internet cannot reach a task. Everything muster needs —
# CloudMap, the ECS API, DynamoDB, ECR, CloudWatch Logs — arrives over VPC
# endpoints, and the test driver reaches PD with `aws ecs execute-command`.

resource "aws_vpc" "main" {
  cidr_block           = var.vpc_cidr
  enable_dns_support   = true
  enable_dns_hostnames = true
  tags = {
    Name = "${local.prefix}main"
  }
}

resource "aws_subnet" "internal" {
  vpc_id            = aws_vpc.main.id
  availability_zone = local.az
  cidr_block        = var.internal_subnet_cidr
  tags = {
    Name = "${local.prefix}internal"
  }
}

resource "aws_route_table" "internal" {
  vpc_id = aws_vpc.main.id
  tags = {
    Name = "${local.prefix}internal"
  }
}

resource "aws_route_table_association" "internal" {
  route_table_id = aws_route_table.internal.id
  subnet_id      = aws_subnet.internal.id
}

# --- security groups -------------------------------------------------------

resource "aws_security_group" "tasks" {
  vpc_id      = aws_vpc.main.id
  name        = "${local.prefix}tasks"
  description = "PD and TiKV Fargate tasks"
  tags = {
    Name = "${local.prefix}tasks"
  }
}

# PD peers, TiKV<->PD and TiKV<->TiKV all talk within this group.
resource "aws_vpc_security_group_ingress_rule" "tasks_self" {
  security_group_id            = aws_security_group.tasks.id
  referenced_security_group_id = aws_security_group.tasks.id
  ip_protocol                  = -1
  description                  = "intra-cluster traffic"
}

# The subnet has no internet route, so blanket egress cannot leave the VPC.
resource "aws_vpc_security_group_egress_rule" "tasks_all" {
  security_group_id = aws_security_group.tasks.id
  cidr_ipv4         = "0.0.0.0/0"
  ip_protocol       = -1
  description       = "VPC endpoints and intra-cluster traffic"
}

resource "aws_security_group" "vpc_endpoint" {
  vpc_id      = aws_vpc.main.id
  name        = "${local.prefix}vpce"
  description = "Interface VPC endpoints"
  tags = {
    Name = "${local.prefix}vpce"
  }
}

resource "aws_vpc_security_group_ingress_rule" "vpc_endpoint_from_tasks" {
  security_group_id            = aws_security_group.vpc_endpoint.id
  referenced_security_group_id = aws_security_group.tasks.id
  ip_protocol                  = "tcp"
  from_port                    = 443
  to_port                      = 443
}

# --- VPC endpoints ---------------------------------------------------------

resource "aws_vpc_endpoint" "s3" {
  vpc_id            = aws_vpc.main.id
  service_name      = "com.amazonaws.${local.aws_region}.s3"
  vpc_endpoint_type = "Gateway"
  route_table_ids   = [aws_route_table.internal.id]
  tags = {
    Name = "${local.prefix}s3"
  }
}

resource "aws_vpc_endpoint" "dynamodb" {
  vpc_id            = aws_vpc.main.id
  service_name      = "com.amazonaws.${local.aws_region}.dynamodb"
  vpc_endpoint_type = "Gateway"
  route_table_ids   = [aws_route_table.internal.id]
  tags = {
    Name = "${local.prefix}dynamodb"
  }
}

locals {
  # Interface endpoints the tasks need:
  #   ecr.api/ecr.dkr + s3  image pulls
  #   logs                  awslogs driver
  #   data-servicediscovery muster's instances()
  #   ecs                   muster's all_ecs_tasks_running()
  #   ssmmessages           ECS Exec — the only way in, so not optional
  interface_endpoints = {
    ecr_api               = "ecr.api"
    ecr_dkr               = "ecr.dkr"
    logs                  = "logs"
    servicediscovery_data = "data-servicediscovery"
    ecs                   = "ecs"
    ssmmessages           = "ssmmessages"
  }
}

resource "aws_vpc_endpoint" "interface" {
  for_each = local.interface_endpoints

  vpc_id              = aws_vpc.main.id
  service_name        = "com.amazonaws.${local.aws_region}.${each.value}"
  vpc_endpoint_type   = "Interface"
  private_dns_enabled = true
  security_group_ids  = [aws_security_group.vpc_endpoint.id]
  subnet_ids          = [aws_subnet.internal.id]
  tags = {
    Name = "${local.prefix}${replace(each.value, ".", "-")}"
  }
}
