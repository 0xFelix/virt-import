# Build the manager binary
ARG BUILD_ARCH=amd64
FROM --platform=linux/${BUILD_ARCH} docker.io/library/golang:1.26 AS builder

ARG TARGETARCH

WORKDIR /workspace
# Copy the Go Modules manifests
COPY go.mod go.mod
COPY go.sum go.sum
COPY go.work go.work
COPY go.work.sum go.work.sum

# Copy the go source
COPY cmd/ cmd/
COPY api/ api/
COPY internal/ internal/
COPY staging/ staging/
COPY vendor/ vendor/

# Build
RUN --mount=type=cache,id="virt-import",target="/root/.cache/go-build" \
    mkdir -p bin && \
    CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} go build -o bin/manager cmd/main.go && \
    CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} go build -o bin/csv-generator ./cmd/csv-generator

# Use distroless as minimal base image to package the manager binary
# Refer to https://github.com/GoogleContainerTools/distroless for more details
FROM --platform=linux/${TARGETARCH} gcr.io/distroless/static:nonroot

# Tells the hyperconverged-cluster-operator where to find csv-generator when it
# runs this image with that binary as the entrypoint.
LABEL org.kubevirt.hco.csv-generator.v1="/csv-generator"

WORKDIR /
COPY --from=builder /workspace/bin/manager .
COPY --from=builder /workspace/bin/csv-generator .
# Rendered by 'make build-csv-manifests'; csv-generator builds the CSV from it.
COPY _out/manifests.yaml /data/manifests.yaml
USER 65532:65532

ENTRYPOINT ["/manager"]
