# End-to-end test: a TiKV cluster (PD + stores) brought up by muster on ECS
# Fargate. See README.md.
#
#   make up        provision everything (terraform + images)
#   make test      run the assertions against an already-provisioned stack
#   make e2e       provision, assert, tear down (what CI would run)
#   make destroy   tear it all down
#
# Override anything below on the command line or in config.mk.

-include config.mk

TERRAFORM ?= terraform
TF_DIR ?= terraform
GO ?= go
DOCKER ?= docker

NAME_PREFIX ?= muster-e2e-tikv-
IMAGE_TAG ?= latest
TIKV_VERSION ?= v8.5.1
PD_DESIRED_COUNT ?= 3
TIKV_DESIRED_COUNT ?= 3

TF_VAR_name_prefix ?= $(NAME_PREFIX)
TF_VAR_image_tag ?= $(IMAGE_TAG)
TF_VAR_pd_desired_count ?= $(PD_DESIRED_COUNT)
TF_VAR_tikv_desired_count ?= $(TIKV_DESIRED_COUNT)

# Only forced when you actually set it. Left alone, the AWS SDK's own resolution
# (AWS_PROFILE, ~/.aws/config) decides the region, which is what terraform and
# the CLI already expect — inventing a default here would silently build the
# stack somewhere other than your configured region.
ifdef AWS_REGION
export AWS_REGION
endif

export TF_VAR_name_prefix
export TF_VAR_image_tag
export TF_VAR_pd_desired_count
export TF_VAR_tikv_desired_count

TF := $(TERRAFORM) -chdir=$(TF_DIR)

# Read out of the Terraform state rather than assembled from an account id and a
# region: the images have to land in the same repositories the task definitions
# point at, and both then come from one source. Recursively expanded, so the
# lookup happens when a recipe needs it — i.e. after `bootstrap` has created the
# repositories — and never for targets that do not.
DOCKER_REGISTRY ?= $(shell $(TF) output -raw ecr_registry 2>/dev/null)

DOCKER_MAKE_ARGS = \
	DOCKER="$(DOCKER)" \
	DOCKER_REGISTRY="$(DOCKER_REGISTRY)" \
	NAME_PREFIX="$(NAME_PREFIX)" \
	IMAGE_TAG="$(IMAGE_TAG)" \
	TIKV_VERSION="$(TIKV_VERSION)"

.PHONY: up
up: init bootstrap images apply

.PHONY: init
init:
	$(TF) init -input=false

# The ECS services cannot be created until their images exist, and the images
# cannot be pushed until the repositories do. Create just the repositories
# first; everything else follows in `apply`.
.PHONY: bootstrap
bootstrap:
	$(TF) apply -input=false -auto-approve \
		-target=aws_ecr_repository.tikv_pd \
		-target=aws_ecr_repository.tikv_node

.PHONY: images
images:
	$(MAKE) -C docker push $(DOCKER_MAKE_ARGS)

.PHONY: apply
apply:
	$(TF) apply -input=false -auto-approve

.PHONY: plan
plan:
	$(TF) plan -input=false

.PHONY: output
output:
	$(TF) output -json

.PHONY: destroy
destroy:
	$(TF) destroy -input=false -auto-approve

# Assertions only: assumes `make up` already ran.
.PHONY: test
test:
	MUSTER_E2E_TIKV=1 MUSTER_E2E_TIKV_PROVISION=0 \
		$(GO) test -tags=e2e_tikv -timeout=120m -v -run TestTiKVOnFargate .

# The whole thing: provision, assert, tear down.
.PHONY: e2e
e2e:
	MUSTER_E2E_TIKV=1 \
		$(GO) test -tags=e2e_tikv -timeout=180m -v -run TestTiKVOnFargate .

.PHONY: logs-pd
logs-pd:
	aws logs tail --follow --since 30m "$(NAME_PREFIX)tikv-pd"

.PHONY: logs-tikv
logs-tikv:
	aws logs tail --follow --since 30m "$(NAME_PREFIX)tikv-node"

.PHONY: clean
clean:
	$(MAKE) -C docker clean $(DOCKER_MAKE_ARGS)
