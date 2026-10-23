# Build the binary
FROM --platform=$BUILDPLATFORM mcr.microsoft.com/oss/go/microsoft/golang:1.26.10-1 AS builder
ARG TARGETOS
ARG TARGETARCH
ARG GIT_COMMIT
ARG VERSION

WORKDIR /workspace
# Copy the Go Modules manifests
COPY go.mod go.mod
COPY go.sum go.sum
# cache deps before building and copying source so that we don't need to re-download as much
# and so that source changes don't invalidate our downloaded layer
ENV GOCACHE=/root/gocache
RUN \
    --mount=type=cache,target=${GOCACHE} \
    --mount=type=cache,target=/go/pkg/mod \
    go mod download

# Copy the go source
COPY cmd/ cmd/
COPY pkg/ pkg/
COPY Makefile Makefile

# Build
# the GOARCH has not a default value to allow the binary be built according to the host where the command
# was called. For example, if we call make docker-build in a local env which has the Apple Silicon M1 SO
# the docker BUILDPLATFORM arg will be linux/arm64 when for Apple x86 it will be linux/amd64. Therefore,
# by leaving it empty we can ensure that the container and binary shipped on it will have the same platform.
RUN --mount=type=cache,target=${GOCACHE} \
    --mount=type=cache,id=keda,sharing=locked,target=/go/pkg/mod \
    MS_GO_NOSYSTEMCRYPTO=1 CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} GO111MODULE=on VERSION=${VERSION} GIT_COMMIT=${GIT_COMMIT} make bin/keda-kaito-scaler

# Build a small image. Azure Linux 3.0 distroless no longer ships the
# compromised emSign ECC Root CA - G3 that cbl-mariner 2.0 still carries.
FROM mcr.microsoft.com/azurelinux/distroless/minimal:3.0
WORKDIR /
COPY --from=builder /workspace/_output/keda-kaito-scaler .

ENTRYPOINT ["/keda-kaito-scaler"]
