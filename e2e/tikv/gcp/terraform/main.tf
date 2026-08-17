terraform {
  required_version = ">=1.9"
  required_providers {
    google = {
      source  = "hashicorp/google"
      version = ">=6.0.0,<8.0.0"
    }
  }
}

provider "google" {
  project = var.project
  region  = var.region
}

data "google_project" "current" {}

locals {
  prefix        = var.name_prefix
  registry_host = "${var.region}-docker.pkg.dev"
  image_repo    = "${local.registry_host}/${var.project}/${google_artifact_registry_repository.images.repository_id}"

  # Service Directory service names the muster scripts discover each other by.
  pd_discovery_name   = "tikv-pd"
  tikv_discovery_name = "tikv-node"

  pd_pool   = "${local.prefix}tikv-pd"
  tikv_pool = "${local.prefix}tikv-node"
  pool_tag  = "${local.prefix}node"

  pd_client_port = 2379
  pd_peer_port   = 2380
  tikv_port      = 20160
  tikv_status    = 20180
}

resource "google_artifact_registry_repository" "images" {
  location      = var.region
  repository_id = trimsuffix(local.prefix, "-")
  format        = "DOCKER"
  description   = "muster TiKV on Cloud Run end-to-end test images"
}

# --- network ---------------------------------------------------------------
#
# The instances reach each other over this VPC at the private addresses Direct
# VPC ingress gives them. That is the whole reason this stack is worker pools
# rather than services or jobs, which get egress without ingress and so cannot
# host anything whose replicas must dial one another.
#
# Google APIs -- Cloud Storage for the seed lease, Service Directory for
# discovery -- go over Cloud Run's own egress rather than this network, because
# vpc_access sends only RFC 1918 traffic here. No Cloud NAT, no Private Google
# Access, and nothing in the stack reachable from outside the VPC.

resource "google_compute_network" "main" {
  name                    = "${local.prefix}net"
  auto_create_subnetworks = false
}

resource "google_compute_subnetwork" "main" {
  name          = "${local.prefix}subnet"
  network       = google_compute_network.main.id
  region        = var.region
  ip_cidr_range = var.subnet_cidr
}

# PD's client and peer ports, TiKV's service and status ports. Without this the
# instances get addresses they cannot use on each other, and the symptom reads
# like a discovery failure rather than a firewall one.
resource "google_compute_firewall" "internal" {
  name          = "${local.prefix}internal"
  network       = google_compute_network.main.name
  direction     = "INGRESS"
  source_ranges = [var.subnet_cidr]
  target_tags   = [local.pool_tag]

  allow {
    protocol = "tcp"
    ports = [
      tostring(local.pd_client_port),
      tostring(local.pd_peer_port),
      tostring(local.tikv_port),
      tostring(local.tikv_status),
    ]
  }
}

# --- the seed lease --------------------------------------------------------

resource "google_storage_bucket" "kv" {
  name     = "${local.prefix}kv-${data.google_project.current.number}"
  location = var.region

  uniform_bucket_level_access = true
  force_destroy               = true

  # Soft delete defaults to seven billed days on buckets created since 2024, and
  # a lease object is rewritten on every renew.
  soft_delete_policy {
    retention_duration_seconds = 0
  }

  # A janitor, not the lease timer: lifecycle age is in whole days. muster
  # filters expiry when it reads a key, which is why a day-granular rule is
  # enough.
  lifecycle_rule {
    condition {
      age            = 1
      matches_prefix = ["leases/"]
    }
    action {
      type = "Delete"
    }
  }
}

# Created only when selected: a database is a slower, heavier and less
# disposable resource than a bucket, so the stack does not stand one up unless
# it is going to be used. Named rather than "(default)", which a project has
# only one of and which cannot be cleanly destroyed.
resource "google_firestore_database" "kv" {
  count = var.kv_backend == "firestore" ? 1 : 0

  project     = var.project
  name        = "${local.prefix}kv"
  location_id = var.region
  type        = "FIRESTORE_NATIVE"

  # Default is ABANDON, which would leave the database behind after
  # `make destroy` -- and this stack is meant to leave nothing.
  deletion_policy = "DELETE"
}

# --- discovery -------------------------------------------------------------

resource "google_service_directory_namespace" "tikv" {
  namespace_id = trimsuffix(local.prefix, "-")
  location     = var.region
}

# Declared empty. Every endpoint under these is written by muster's register()
# builtin, because nothing on Cloud Run registers an instance -- so that they
# appear at all is one of the things the test asserts.
resource "google_service_directory_service" "pd" {
  service_id = local.pd_discovery_name
  namespace  = google_service_directory_namespace.tikv.id
}

resource "google_service_directory_service" "tikv" {
  service_id = local.tikv_discovery_name
  namespace  = google_service_directory_namespace.tikv.id
}

# --- identity --------------------------------------------------------------

resource "google_service_account" "node" {
  account_id   = "${trimsuffix(local.prefix, "-")}-node"
  display_name = "muster TiKV on Cloud Run end-to-end test"
}

# editor, not viewer: the instances create and delete their own endpoints.
resource "google_service_directory_namespace_iam_member" "editor" {
  name   = google_service_directory_namespace.tikv.id
  role   = "roles/servicedirectory.editor"
  member = "serviceAccount:${google_service_account.node.email}"
}

resource "google_storage_bucket_iam_member" "kv" {
  bucket = google_storage_bucket.kv.name
  role   = "roles/storage.objectUser"
  member = "serviceAccount:${google_service_account.node.email}"
}

# Firestore has no per-database IAM, so this is necessarily project-scoped.
# Granted only when the backend is actually in use.
resource "google_project_iam_member" "firestore" {
  count = var.kv_backend == "firestore" ? 1 : 0

  project = var.project
  role    = "roles/datastore.user"
  member  = "serviceAccount:${google_service_account.node.email}"
}

resource "google_artifact_registry_repository_iam_member" "pull" {
  location   = google_artifact_registry_repository.images.location
  repository = google_artifact_registry_repository.images.name
  role       = "roles/artifactregistry.reader"
  member     = "serviceAccount:${google_service_account.node.email}"
}

# --- the pools -------------------------------------------------------------

locals {
  # -kv-store names the bucket or the collection, and -provider-opt selects
  # between them. Both travel as MUSTER_<FLAG>; see the PD pool below.
  kv_store = var.kv_backend == "firestore" ? "${local.prefix}leases" : google_storage_bucket.kv.name

  kv_provider_opt = var.kv_backend == "firestore" ? format(
    "kv.backend=firestore,kv.database=%s", google_firestore_database.kv[0].name,
  ) : ""

  common_env = {
    # MUSTER_PROVIDER is not set on purpose: Cloud Run sets
    # CLOUD_RUN_WORKER_POOL, so muster's autodetection finds the provider on
    # its own, and that it does is part of what this stack exercises.
    MUSTER_PD_SERVICE   = local.pd_discovery_name
    MUSTER_TIKV_SERVICE = local.tikv_discovery_name
    MUSTER_PD_REPLICAS  = tostring(var.pd_instance_count)
    # pd.star will not act on a discovery answer that does not account for a
    # majority of this many stores.
    MUSTER_TIKV_REPLICAS = tostring(var.tikv_instance_count)
  }
}

resource "google_cloud_run_v2_worker_pool" "pd" {
  name         = local.pd_pool
  location     = var.region
  launch_stage = "BETA"

  # Without this, `terraform destroy` refuses.
  deletion_protection = false

  scaling {
    # A fixed count, not autoscaling: a Raft group needs a known, odd number of
    # members, and an autoscaler deciding otherwise would be a split brain
    # waiting to happen.
    scaling_mode          = "MANUAL"
    manual_instance_count = var.pd_instance_count
  }

  template {
    service_account = google_service_account.node.email

    vpc_access {
      network_interfaces {
        network    = google_compute_network.main.id
        subnetwork = google_compute_subnetwork.main.id
        tags       = [local.pool_tag]
      }
      # Only RFC 1918 traffic goes to the VPC; Google API traffic takes Cloud
      # Run's own egress. Peer traffic, which is what matters, is private by
      # definition.
      egress = "PRIVATE_RANGES_ONLY"
    }

    volumes {
      name = "data"
      empty_dir {
        # DISK, not the default MEMORY. An in-memory volume is charged against
        # the instance's memory limit, so PD's data directory would eat the RAM
        # it needs to run -- and 10Gi of it would not fit at all.
        #
        # It is still ephemeral: the contents go when the instance does. That is
        # the same deal Fargate offers, which is why the script derives its
        # member name from the address and evicts itself in pre_stop.
        medium     = "DISK"
        size_limit = var.data_disk_size
      }
    }

    containers {
      image = "${local.image_repo}/tikv-pd:${var.image_tag}"

      volume_mounts {
        name       = "data"
        mount_path = "/pd"
      }

      resources {
        limits = {
          cpu    = var.pd_cpu
          memory = var.pd_memory
        }
      }

      dynamic "env" {
        # MUSTER_NAMESPACE and MUSTER_KV_STORE are muster's own -namespace and
        # -kv-store. Every flag can be given as MUSTER_<FLAG>, which is what
        # lets the image keep its entrypoint: it ends in `-- /pd-server`, so
        # appended arguments would land after the separator and become part of
        # COMMAND rather than flags.
        #
        # The names have to be the flags' own. An earlier version set
        # MUSTER_KV_BUCKET, which is not what the flag is called, so the setting
        # simply did not apply -- see TestE2EStackEnvIsConsumed.
        for_each = merge(local.common_env, {
          MUSTER_NAMESPACE = google_service_directory_namespace.tikv.namespace_id
          MUSTER_KV_STORE  = local.kv_store
          MUSTER_DATA_DIR  = "/pd"
          }, local.kv_provider_opt == "" ? {} : {
          # A repeatable flag takes a comma-separated list in its variable.
          MUSTER_PROVIDER_OPT = local.kv_provider_opt
        })
        content {
          name  = env.key
          value = env.value
        }
      }
    }
  }

  depends_on = [
    google_storage_bucket_iam_member.kv,
    google_service_directory_namespace_iam_member.editor,
    google_artifact_registry_repository_iam_member.pull,
    google_service_directory_service.pd,
    google_compute_firewall.internal,
  ]
}

resource "google_cloud_run_v2_worker_pool" "tikv" {
  name         = local.tikv_pool
  location     = var.region
  launch_stage = "BETA"

  deletion_protection = false

  scaling {
    scaling_mode          = "MANUAL"
    manual_instance_count = var.tikv_instance_count
  }

  template {
    service_account = google_service_account.node.email

    vpc_access {
      network_interfaces {
        network    = google_compute_network.main.id
        subnetwork = google_compute_subnetwork.main.id
        tags       = [local.pool_tag]
      }
      egress = "PRIVATE_RANGES_ONLY"
    }

    volumes {
      name = "data"
      empty_dir {
        medium     = "DISK"
        size_limit = var.data_disk_size
      }
    }

    containers {
      image = "${local.image_repo}/tikv-node:${var.image_tag}"

      volume_mounts {
        name       = "data"
        mount_path = "/db"
      }

      resources {
        limits = {
          cpu    = var.tikv_cpu
          memory = var.tikv_memory
        }
      }

      dynamic "env" {
        # No MUSTER_KV_STORE: only PD runs the seed election, so a kv_* call
        # from tikv.star raises rather than quietly using a store it was never
        # meant to touch.
        for_each = merge(local.common_env, {
          MUSTER_NAMESPACE = google_service_directory_namespace.tikv.namespace_id
          MUSTER_DATA_DIR  = "/db"
        })
        content {
          name  = env.key
          value = env.value
        }
      }
    }
  }

  # The stores wait for PD themselves, but starting them into a region with no
  # PD at all only produces noise.
  depends_on = [
    google_cloud_run_v2_worker_pool.pd,
    google_storage_bucket_iam_member.kv,
    google_service_directory_namespace_iam_member.editor,
    google_artifact_registry_repository_iam_member.pull,
    google_service_directory_service.tikv,
    google_compute_firewall.internal,
  ]
}
