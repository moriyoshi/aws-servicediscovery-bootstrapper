# End-to-end test: a TiKV cluster (PD + stores) brought up by muster on Cloud
# Run worker pools. See README.md.
#
#   make up        provision everything (terraform + image)
#   make test      run the assertions against an already-provisioned stack
#   make e2e       provision, assert, tear down (what CI would run)
#   make destroy   tear it all down

-include config.mk

TERRAFORM ?= terraform
TF_DIR ?= terraform
GO ?= go
DOCKER ?= docker
GCLOUD ?= gcloud

NAME_PREFIX ?= muster-e2e-tikv-
IMAGE_TAG ?= latest
TIKV_VERSION ?= v8.5.1
PD_COUNT ?= 3
TIKV_COUNT ?= 3
# gcs (a bucket) or firestore (a collection + a database).
KV_BACKEND ?= gcs

GCP_PROJECT ?= $(shell $(GCLOUD) config get-value project 2>/dev/null)
GCP_REGION ?= asia-northeast1

TF_VAR_project ?= $(GCP_PROJECT)
TF_VAR_region ?= $(GCP_REGION)
TF_VAR_name_prefix ?= $(NAME_PREFIX)
TF_VAR_image_tag ?= $(IMAGE_TAG)
TF_VAR_pd_instance_count ?= $(PD_COUNT)
TF_VAR_tikv_instance_count ?= $(TIKV_COUNT)
TF_VAR_kv_backend ?= $(KV_BACKEND)

export TF_VAR_project
export TF_VAR_region
export TF_VAR_name_prefix
export TF_VAR_image_tag
export TF_VAR_pd_instance_count
export TF_VAR_tikv_instance_count
export TF_VAR_kv_backend

TF := $(TERRAFORM) -chdir=$(TF_DIR)

# Derived here rather than read back from `terraform output`.
#
# `make bootstrap` is a *targeted* apply, and Terraform does not necessarily
# evaluate outputs during one -- so reading them straight afterwards can yield
# nothing, and an empty value reaches `docker login https://`, which is not a
# registry. The push then goes out anonymously and the registry reports a
# permissions problem on a resource that "may not exist", naming neither.
#
# Both are pure functions of the project, region and prefix, and Terraform
# computes them the same way, so there is nothing to read back.
#
# One value, not a separate registry host beside it: `docker login` normalizes a
# path-qualified address down to its hostname -- verified, the credential lands
# under "<region>-docker.pkg.dev" either way -- so a second variable would buy
# nothing and could only drift from this one.
AR_REPOSITORY ?= $(patsubst %-,%,$(NAME_PREFIX))
IMAGE_REPO ?= $(TF_VAR_region)-docker.pkg.dev/$(TF_VAR_project)/$(AR_REPOSITORY)

DOCKER_MAKE_ARGS = \
	DOCKER="$(DOCKER)" \
	IMAGE_REPO="$(IMAGE_REPO)" \
	IMAGE_TAG="$(IMAGE_TAG)" \
	TIKV_VERSION="$(TIKV_VERSION)" \
	GCP_PROJECT="$(TF_VAR_project)" \
	GCP_REGION="$(TF_VAR_region)" \
	AR_REPOSITORY="$(AR_REPOSITORY)"

.PHONY: check-project
check-project:
	@test -n "$(TF_VAR_project)" || { \
		echo "No project. Set GCP_PROJECT in config.mk or on the command line," >&2; \
		echo "or run 'gcloud config set project <id>'." >&2; \
		exit 1; }

.PHONY: up
up: init bootstrap images apply

.PHONY: init
init:
	$(TF) init -input=false

# The job names an image that does not exist yet, and the image cannot be
# pushed until the repository does.
.PHONY: bootstrap
bootstrap: check-project
	$(TF) apply -input=false -auto-approve \
		-target=google_artifact_registry_repository.images

.PHONY: images
images: check-project
	$(MAKE) -C docker push $(DOCKER_MAKE_ARGS)

.PHONY: apply
apply: check-project
	$(TF) apply -input=false -auto-approve

.PHONY: plan
plan: check-project
	$(TF) plan -input=false

.PHONY: output
output:
	$(TF) output -json

.PHONY: destroy
destroy: check-project
	$(TF) destroy -input=false -auto-approve

.PHONY: test
test:
	MUSTER_E2E_TIKV_GCP=1 MUSTER_E2E_TIKV_GCP_PROVISION=0 \
		$(GO) test -tags=e2e_tikv_gcp -timeout=120m -v -run TestTiKVOnCloudRun .

.PHONY: e2e
e2e:
	MUSTER_E2E_TIKV_GCP=1 \
		$(GO) test -tags=e2e_tikv_gcp -timeout=180m -v -run TestTiKVOnCloudRun .

.PHONY: logs-pd
logs-pd: check-project
	$(GCLOUD) logging read \
		'resource.type=cloud_run_worker AND resource.labels.worker_pool_name="$(NAME_PREFIX)tikv-pd"' \
		--project="$(TF_VAR_project)" --limit=200 --freshness=30m --order=asc

.PHONY: logs-tikv
logs-tikv: check-project
	$(GCLOUD) logging read \
		'resource.type=cloud_run_worker AND resource.labels.worker_pool_name="$(NAME_PREFIX)tikv-node"' \
		--project="$(TF_VAR_project)" --limit=200 --freshness=30m --order=asc

.PHONY: clean
clean:
	$(MAKE) -C docker clean $(DOCKER_MAKE_ARGS)
