# Build the manager binary
FROM golang:1.23-alpine AS builder
ARG TARGETOS
ARG TARGETARCH

ARG RELEASE_VERSION=master
ARG RELEASE_COMMIT=none
ARG RELEASE_DATE=unknown

WORKDIR /workspace
# Copy the Go Modules manifests
COPY go.mod go.mod
COPY go.sum go.sum
# cache deps before building and copying source so that we don't need to re-download as much
# and so that source changes don't invalidate our downloaded layer
RUN go mod download

# Copy the go source
COPY cmd/main.go cmd/main.go
COPY api/ api/
COPY internal/ internal/

# Build
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH GO111MODULE=on go build -ldflags="-s -w -X 'main.version=$RELEASE_VERSION' -X 'main.commit=$RELEASE_COMMIT' -X 'main.date=$RELEASE_DATE'" -a -o manager cmd/main.go

FROM scratch
LABEL "name"="humio-operator"
LABEL "vendor"="humio"
LABEL "summary"="Humio Kubernetes Operator"
LABEL "description"="A Kubernetes operatator to run and maintain \
Humio clusters running in a Kubernetes cluster."

COPY LICENSE /licenses/LICENSE

WORKDIR /
COPY --from=builder /workspace/manager .
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt

USER 1001

ENTRYPOINT ["/manager"]