# Dev container: every tool needed to generate, lint and test the module.
# All versions are pinned; bump them here and nowhere else. Run commands in it
# with hack/dev.sh. The versions match those of sc-karpenter-nodepool.
FROM golang:1.26.9-bookworm

ARG TARGETARCH
ARG CONTROLLER_GEN_VERSION=v0.22.0
ARG GOLANGCI_LINT_VERSION=v2.14.0

SHELL ["/bin/bash", "-euxo", "pipefail", "-c"]

RUN apt-get update \
 && apt-get install -y --no-install-recommends make git ca-certificates curl \
 && rm -rf /var/lib/apt/lists/*

RUN curl -fsSL "https://github.com/golangci/golangci-lint/releases/download/${GOLANGCI_LINT_VERSION}/golangci-lint-${GOLANGCI_LINT_VERSION#v}-linux-${TARGETARCH}.tar.gz" \
    | tar -xz --strip-components=1 -C /usr/local/bin "golangci-lint-${GOLANGCI_LINT_VERSION#v}-linux-${TARGETARCH}/golangci-lint"

ENV GOFLAGS=-buildvcs=false
RUN GOBIN=/usr/local/bin go install "sigs.k8s.io/controller-tools/cmd/controller-gen@${CONTROLLER_GEN_VERSION}" \
 && go clean -cache -modcache \
 && git config --system --add safe.directory /src

# Mount point of the cache volume (see hack/dev.sh): world-writable so the
# volume is usable by whatever uid the container runs as.
RUN mkdir -p /cache && chmod 1777 /cache

WORKDIR /src
