variable "project" {
  type        = string
  description = "Project to build the stack in. Required: there is no safe default for something that creates billable resources."
}

variable "region" {
  type        = string
  description = "Region for the pools, the subnet, the Service Directory namespace and the bucket."
  default     = "asia-northeast1"
}

variable "name_prefix" {
  type        = string
  description = "Prefix applied to every resource name, so several stacks can coexist in one project."
  default     = "muster-e2e-tikv-"
}

variable "subnet_cidr" {
  type        = string
  description = <<-EOT
    CIDR the pools' instances get their addresses from. They reach each other
    here and nowhere else: no external address, no NAT, no load balancer.
  EOT
  default     = "10.128.253.0/24"
}

variable "image_tag" {
  type    = string
  default = "latest"
}

variable "pd_instance_count" {
  type        = number
  description = "PD replicas. Must be odd for a Raft quorum."
  default     = 3

  validation {
    condition     = var.pd_instance_count % 2 == 1 && var.pd_instance_count >= 1
    error_message = "pd_instance_count must be an odd, positive number."
  }
}

variable "tikv_instance_count" {
  type        = number
  description = "TiKV stores. Must be at least the replication factor (3)."
  default     = 3

  validation {
    condition     = var.tikv_instance_count >= 3
    error_message = "tikv_instance_count must be at least 3, PD's default max-replicas."
  }
}

variable "kv_backend" {
  type        = string
  description = <<-EOT
    Which store backs the kv_* builtins: "gcs" (a bucket) or "firestore" (a
    collection). Both satisfy the same conformance suite, so this exercises the
    choice rather than changing what the cluster does.

    Firestore additionally creates a database, which is a heavier and slower
    resource than a bucket -- that asymmetry is the reason gcs is the default.
  EOT
  default     = "gcs"

  validation {
    condition     = contains(["gcs", "firestore"], var.kv_backend)
    error_message = "kv_backend must be gcs or firestore."
  }
}

variable "data_disk_size" {
  type        = string
  description = <<-EOT
    Ephemeral disk per instance. 10Gi is the minimum Cloud Run accepts and the
    per-instance default maximum; more needs a quota increase.

    Ephemeral is the operative word: the contents are deleted when the instance
    shuts down, so this is Fargate's deal rather than a persistent volume. The
    scripts are written for that.
  EOT
  default     = "10Gi"
}

variable "pd_cpu" {
  type    = string
  default = "1"
}

variable "pd_memory" {
  type    = string
  default = "2Gi"
}

variable "tikv_cpu" {
  type    = string
  default = "2"
}

variable "tikv_memory" {
  type        = string
  description = "TiKV wants memory; 4Gi is the smallest that reliably holds a store."
  default     = "4Gi"
}
