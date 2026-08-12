variable "name_prefix" {
  type        = string
  description = "Prefix applied to every resource name, so several stacks can coexist in one account."
  default     = "muster-e2e-tikv-"
}

variable "vpc_cidr" {
  type        = string
  description = "CIDR of the test VPC."
  default     = "172.31.254.0/23"
}

variable "internal_subnet_cidr" {
  type        = string
  description = <<-EOT
    CIDR of the isolated subnet that hosts the Fargate tasks. Nothing in this
    stack is reachable from outside the VPC: there is no internet gateway and no
    load balancer, so the subnet has no route off the VPC at all and everything
    the tasks need is reached over VPC endpoints. The muster scripts also take
    it as the fallback for their own address, for when the task metadata
    endpoint leaves SELF.ipv4 empty.
  EOT
  default     = "172.31.255.0/24"
}

variable "image_tag" {
  type        = string
  description = "Tag of the tikv-pd / tikv-node images to run."
  default     = "latest"
}

variable "pd_desired_count" {
  type        = number
  description = "Number of PD replicas. Must be odd for a Raft quorum."
  default     = 3

  validation {
    condition     = var.pd_desired_count % 2 == 1 && var.pd_desired_count >= 1
    error_message = "pd_desired_count must be an odd, positive number."
  }
}

variable "tikv_desired_count" {
  type        = number
  description = "Number of TiKV stores. Must be at least the replication factor (3)."
  default     = 3

  validation {
    condition     = var.tikv_desired_count >= 3
    error_message = "tikv_desired_count must be at least 3, PD's default max-replicas."
  }
}

variable "pd_cpu" {
  type    = number
  default = 512
}

variable "pd_memory" {
  type    = number
  default = 1024
}

variable "tikv_cpu" {
  type    = number
  default = 1024
}

variable "tikv_memory" {
  type    = number
  default = 4096
}

variable "log_retention_days" {
  type        = number
  description = "Retention of the task log groups. Short by default; this is throwaway infrastructure."
  default     = 3
}

variable "wait_for_steady_state" {
  type        = bool
  description = "Block `terraform apply` until both ECS services reach steady state."
  default     = true
}
