# Builds the two node images: muster from the working tree (with -tags gcp),
# layered onto the upstream PD and TiKV images. Driven by ../Makefile, which
# supplies IMAGE_REPO; every variable here can also be overridden on the command
# line or in ../config.mk.

-include ../config.mk

DOCKER ?= docker
DOCKER_BUILD_ARGS ?=
PLATFORM ?= linux/amd64
IMAGE_TAG ?= latest
TIKV_VERSION ?= v8.5.1
GO_VERSION ?= 1.26

# Repository root: the muster build context.
ROOT := ../../../..

MUSTER_IMAGE := muster:e2e-gcp
IMAGE_TIKV_PD := $(IMAGE_REPO)/tikv-pd
IMAGE_TIKV_NODE := $(IMAGE_REPO)/tikv-node

.PHONY: all
all: tikv-pd tikv-node

# An empty value means ../Makefile could not work out where to push, which in
# practice means the project is unset.
.PHONY: check-registry
check-registry:
	@test -n "$(IMAGE_REPO)" || { \
		echo "IMAGE_REPO is empty." >&2; \
		echo "Run 'make up' from e2e/tikv/gcp (which runs 'make bootstrap' first)," >&2; \
		echo "or set them in e2e/tikv/gcp/config.mk to push elsewhere." >&2; \
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
		tikv-pd

.PHONY: tikv-node
tikv-node: check-registry muster
	$(DOCKER) build $(DOCKER_BUILD_ARGS) \
		--platform="$(PLATFORM)" \
		--build-arg=TIKV_VERSION="$(TIKV_VERSION)" \
		--build-arg=MUSTER_IMAGE="$(MUSTER_IMAGE)" \
		-t "$(IMAGE_TIKV_NODE):$(IMAGE_TAG)" \
		tikv-node

# Log in with a short-lived access token rather than wiring gcloud's credential
# helper into ~/.docker/config.json.
#
# `gcloud auth configure-docker <host>` is the blessed route, but it depends on
# docker-credential-gcloud being invocable by Docker and on the host actually
# reaching credHelpers -- and when any of that does not hold, Docker pushes
# *anonymously*. The registry then reports "Unauthenticated requests do not have
# permission ... (or it may not exist)", which reads as a missing IAM grant or a
# missing repository rather than a missing credential. This is explicit, works
# regardless of helper state, and does not edit global Docker config as a side
# effect of running a test.
#
# IMAGE_REPO is path-qualified and that is fine: docker login normalizes the
# address to its hostname, which is the key a push looks up.
.PHONY: auth
auth: check-registry
	@gcloud auth print-access-token >/dev/null 2>&1 || { \
		echo "gcloud has no active credential to mint a token from." >&2; \
		echo "Run 'gcloud auth login'. Note that 'gcloud auth application-default" >&2; \
		echo "login' is not enough: that sets ADC for the workload, not a token" >&2; \
		echo "for this push." >&2; \
		exit 1; }
	@gcloud auth print-access-token | \
		$(DOCKER) login -u oauth2accesstoken --password-stdin "https://$(IMAGE_REPO)"

# The repository has to exist before a push, and a push to a missing one fails
# with the same "(or it may not exist)" message as an auth failure. Checking
# separates the two.
#
# --location is right, whatever a nearby Cloud Run command suggests: Artifact
# Registry and Service Directory are regional resources addressed by --location,
# while `gcloud run` takes --region. Passing --region here is what produces
# "unrecognized arguments".
#
# gcloud's own error is printed rather than swallowed, and only a genuine
# not-found is blamed on a missing repository. Discarding it cost a provision:
# terraform had just reported the repository present, this said it did not
# exist, and the actual cause was an expired credential -- which the message
# named nowhere, because it could only ever say one thing.
.PHONY: check-repo
check-repo: check-registry
	@out=$$(gcloud artifacts repositories describe "$(AR_REPOSITORY)" \
		--project="$(GCP_PROJECT)" --location="$(GCP_REGION)" 2>&1) || { \
		echo "$$out" >&2; \
		echo "" >&2; \
		case "$$out" in \
		*"not found"*|*NOT_FOUND*|*"does not exist"*) \
			echo "Artifact Registry repository '$(AR_REPOSITORY)' does not exist in" >&2; \
			echo "$(GCP_PROJECT)/$(GCP_REGION). Run 'make bootstrap' from e2e/tikv/gcp." >&2 ;; \
		*"Reauthentication"*|*"auth"*|*"credential"*) \
			echo "gcloud could not authenticate. Run 'gcloud auth login' -- a session" >&2; \
			echo "that expires mid-run reports here first, because this is the first" >&2; \
			echo "thing after terraform to call the API." >&2 ;; \
		*) \
			echo "Could not check the Artifact Registry repository. The error above is" >&2; \
			echo "gcloud's own; this is not necessarily a missing repository." >&2 ;; \
		esac; \
		exit 1; }

.PHONY: push
push: check-repo auth all
	$(DOCKER) push "$(IMAGE_TIKV_PD):$(IMAGE_TAG)"
	$(DOCKER) push "$(IMAGE_TIKV_NODE):$(IMAGE_TAG)"

.PHONY: clean
clean:
	-$(DOCKER) rmi -f \
		"$(IMAGE_TIKV_PD):$(IMAGE_TAG)" \
		"$(IMAGE_TIKV_NODE):$(IMAGE_TAG)" \
		"$(MUSTER_IMAGE)"
