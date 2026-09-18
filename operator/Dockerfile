# Build the manager, bundle-service and token-broker binaries.
#
# Both ship in this single image, selected per-Deployment via an explicit
# `command:` (see charts/operator/templates/). ENTRYPOINT stays /manager so
# existing consumers that run the image with no command are unaffected.
#
# `--platform=$BUILDPLATFORM` pins the builder stage to the host arch so
# Go cross-compiles to $TARGETARCH instead of running under QEMU. With
# CGO disabled (below), the resulting binary is bit-for-bit identical to
# a native build but ~9x faster for the arm64 variant.
FROM --platform=$BUILDPLATFORM docker.io/golang:1.26 AS builder
ARG TARGETOS
ARG TARGETARCH

WORKDIR /workspace
# Copy the Go Modules manifests
COPY go.mod go.mod
COPY go.sum go.sum
# cache deps before building and copying source so that we don't need to re-download as much
# and so that source changes don't invalidate our downloaded layer
RUN go mod download

# Copy the go source
COPY cmd/main.go cmd/main.go
COPY cmd/bundle-service/ cmd/bundle-service/
COPY cmd/token-broker/ cmd/token-broker/
COPY api/ api/
COPY internal/ internal/
COPY pkg/ pkg/

# Build
# the GOARCH has not a default value to allow the binary be built according to the host where the command
# was called. For example, if we call make docker-build in a local env which has the Apple Silicon M1 SO
# the docker BUILDPLATFORM arg will be linux/arm64 when for Apple x86 it will be linux/amd64. Therefore,
# by leaving it empty we can ensure that the container and binary shipped on it will have the same platform.
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build -o manager cmd/main.go

# bundle-service serves OPA authorization policy bundles. Same module, so it
# reuses the layers above; only this stage's output differs.
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build -o bundle-service ./cmd/bundle-service/

# token-broker brokers OAuth flows and caches tokens for AuthBridge.
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build -o token-broker ./cmd/token-broker/

# Use distroless as minimal base image to package the binaries
# Refer to https://github.com/GoogleContainerTools/distroless for more details
FROM gcr.io/distroless/static:nonroot
WORKDIR /
COPY --from=builder /workspace/manager .
COPY --from=builder /workspace/bundle-service .
COPY --from=builder /workspace/token-broker .
USER 65532:65532

ENTRYPOINT ["/manager"]
