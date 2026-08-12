# Builds the two task images: muster from the working tree, layered onto the
# upstream PD and TiKV images. Driven by ../Makefile, which supplies
# DOCKER_REGISTRY; every variable here can also be overridden on the command
# line or in ../config.mk.

-include ../config.mk

DOCKER ?= docker
DOCKER_BUILD_ARGS ?=
PLATFORM ?= linux/amd64
NAME_PREFIX ?= muster-e2e-tikv-
IMAGE_TAG ?= latest
TIKV_VERSION ?= v8.5.1
GO_VERSION ?= 1.25

# Repository root: the muster build context.
ROOT := ../../..

MUSTER_IMAGE := muster:e2e
IMAGE_TIKV_PD := $(DOCKER_REGISTRY)/$(NAME_PREFIX)tikv-pd
IMAGE_TIKV_NODE := $(DOCKER_REGISTRY)/$(NAME_PREFIX)tikv-node

.PHONY: all
all: tikv-pd tikv-node

# The registry normally comes from ../Makefile, which reads it out of the
# Terraform state; an empty value almost always means `make bootstrap` has not
# created the ECR repositories yet.
.PHONY: check-registry
check-registry:
	@test -n "$(DOCKER_REGISTRY)" || { \
		echo "DOCKER_REGISTRY is empty." >&2; \
		echo "Run 'make up' from e2e/tikv (which runs 'make bootstrap' first)," >&2; \
		echo "or set DOCKER_REGISTRY in e2e/tikv/config.mk to push elsewhere." >&2; \
		exit 1; }

.PHONY: muster
muster:
	$(DOCKER) build $(DOCKER_BUILD_ARGS) \
		--platform="$(PLATFORM)" \
		--build-arg=GO_VERSION="$(GO_VERSION)" \
		-f muster/Dockerfile \
		-t "$(MUSTER_IMAGE)" \
		"$(ROOT)"

.PHONY: tikv-pd
tikv-pd: check-registry muster
	$(DOCKER) build $(DOCKER_BUILD_ARGS) \
		--platform="$(PLATFORM)" \
		--build-arg=TIKV_VERSION="$(TIKV_VERSION)" \
		--build-arg=MUSTER_IMAGE="$(MUSTER_IMAGE)" \
		-t "$(IMAGE_TIKV_PD):$(IMAGE_TAG)" \
		-t "$(IMAGE_TIKV_PD):$(TIKV_VERSION)" \
		tikv-pd

.PHONY: tikv-node
tikv-node: check-registry muster
	$(DOCKER) build $(DOCKER_BUILD_ARGS) \
		--platform="$(PLATFORM)" \
		--build-arg=TIKV_VERSION="$(TIKV_VERSION)" \
		--build-arg=MUSTER_IMAGE="$(MUSTER_IMAGE)" \
		-t "$(IMAGE_TIKV_NODE):$(IMAGE_TAG)" \
		-t "$(IMAGE_TIKV_NODE):$(TIKV_VERSION)" \
		tikv-node

# Taken from the registry hostname (<account>.dkr.ecr.<region>.amazonaws.com) so
# the login region cannot drift from the registry it is a login for.
ECR_REGION ?= $(shell echo "$(DOCKER_REGISTRY)" | cut -d. -f4)

.PHONY: login
login: check-registry
	aws ecr get-login-password --region "$(ECR_REGION)" \
		| $(DOCKER) login --username AWS --password-stdin "$(DOCKER_REGISTRY)"

.PHONY: push
push: login tikv-pd tikv-node
	$(DOCKER) push "$(IMAGE_TIKV_PD):$(IMAGE_TAG)"
	$(DOCKER) push "$(IMAGE_TIKV_PD):$(TIKV_VERSION)"
	$(DOCKER) push "$(IMAGE_TIKV_NODE):$(IMAGE_TAG)"
	$(DOCKER) push "$(IMAGE_TIKV_NODE):$(TIKV_VERSION)"

.PHONY: clean
clean:
	-$(DOCKER) rmi -f \
		"$(IMAGE_TIKV_PD):$(IMAGE_TAG)" \
		"$(IMAGE_TIKV_PD):$(TIKV_VERSION)" \
		"$(IMAGE_TIKV_NODE):$(IMAGE_TAG)" \
		"$(IMAGE_TIKV_NODE):$(TIKV_VERSION)" \
		"$(MUSTER_IMAGE)"
