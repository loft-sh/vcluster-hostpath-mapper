# Build the manager binary
FROM golang:1.24.2 as builder

WORKDIR /vcluster-hpm-dev
ARG TARGETOS
ARG TARGETARCH
ARG BUILD_VERSION=dev
ARG TELEMETRY_PRIVATE_KEY=""

# Install kubectl for development
RUN curl -LO https://storage.googleapis.com/kubernetes-release/release/`curl -s https://storage.googleapis.com/kubernetes-release/release/stable.txt`/bin/linux/amd64/kubectl && chmod +x ./kubectl && mv ./kubectl /usr/local/bin/kubectl

# Install Delve for debugging
RUN if [ "${TARGETARCH}" = "amd64" ] || [ "${TARGETARCH}" = "arm64" ]; then go install github.com/go-delve/delve/cmd/dlv@latest; fi

# Copy the Go Modules manifests
COPY go.mod go.mod
COPY go.sum go.sum
COPY vendor/ vendor/

# Copy the go source
COPY cmd/ cmd/

# Symlink /manifests folder to the synced location for development purposes
# RUN ln -s "$(pwd)/manifests" /manifests

ENV GO111MODULE on
ENV DEBUG true

# create and set GOCACHE now, this should slightly speed up the first build inside of the container
# also create /.config folder for GOENV, as dlv needs to write there when starting debugging
RUN mkdir -p /.cache /.config
ENV GOCACHE=/.cache
ENV GOENV=/.config

# Set home to "/" in order to for kubectl to automatically pick up vcluster kube config 
ENV HOME /

# Build cmd
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} GO111MODULE=on go build -mod vendor -o /vcluster-hpm cmd/main.go

# RUN useradd -u 12345 nonroot
# USER nonroot

ENTRYPOINT ["go", "run", "-mod", "vendor", "cmd/main.go"]

# we use alpine for easier debugging
FROM alpine:3.18

# Set root path as working directory
WORKDIR /

COPY --from=builder /vcluster-hpm .

# RUN useradd -u 12345 nonroot
# USER nonroot

ENTRYPOINT ["/vcluster-hpm"]
