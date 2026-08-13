# syntax=docker/dockerfile:1
#
# Builds the muster binary the task images layer on top of. The build context is
# the repository root, so the image always carries the working tree's muster
# rather than a published release.

ARG GO_VERSION=1.26

FROM --platform=${BUILDPLATFORM} public.ecr.aws/docker/library/golang:${GO_VERSION}-bookworm AS build
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64} \
    go build -trimpath -ldflags='-s -w' -o /out/muster .

FROM scratch
COPY --from=build /out/muster /muster
