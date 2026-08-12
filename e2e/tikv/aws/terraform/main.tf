terraform {
  required_version = ">=1.9"
  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = ">=5.97.0,<6.0.0"
    }
  }
}

provider "aws" {}

data "aws_region" "current" {}
data "aws_caller_identity" "current" {}
data "aws_partition" "current" {}
data "aws_availability_zones" "available" {
  state = "available"
}

locals {
  aws_account_id = data.aws_caller_identity.current.account_id
  aws_partition  = data.aws_partition.current.partition
  aws_region     = data.aws_region.current.name
  az             = data.aws_availability_zones.available.names[0]
  root_user_arn  = "arn:${local.aws_partition}:iam::${local.aws_account_id}:root"

  prefix = var.name_prefix

  # CloudMap service names the muster scripts discover each other by. These
  # are the Service Connect `discovery_name`s, not the ECS service names.
  pd_discovery_name   = "tikv-pd"
  tikv_discovery_name = "tikv-node"

  # ECS service names. Literals rather than references to the services
  # themselves: the task definitions pass these to the scripts, and a task
  # definition that referenced its own service would be a dependency cycle.
  pd_service_name   = "tikv-pd"
  tikv_service_name = "tikv-node"
}
