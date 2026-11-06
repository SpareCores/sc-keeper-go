#!/usr/bin/env bash
# Run a command inside the dev container (hack/devcontainer/Dockerfile).
#
#   hack/dev.sh make test
#   hack/dev.sh go test ./selection/...
#
# The image is built on first use and rebuilt when the Dockerfile changes. The
# repository is mounted at /src; Go module and build caches live in a named
# volume.
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
dockerfile="${repo_root}/hack/devcontainer/Dockerfile"

image_repo="${SCKG_DEV_IMAGE_REPO:-sc-keeper-go-dev}"
cache_volume="${SCKG_DEV_CACHE_VOLUME:-sc-keeper-go-dev-cache}"
tag="$(sha256sum "${dockerfile}" | cut -c1-12)"
image="${image_repo}:${tag}"

if [[ "${1:-}" == "--build" ]]; then
  # Used by `make dev-image`: only make sure the image exists.
  shift
  want_build_only=1
else
  want_build_only=0
fi

if ! docker image inspect "${image}" >/dev/null 2>&1; then
  docker build -t "${image}" -f "${dockerfile}" "${repo_root}/hack/devcontainer"
fi

if [[ "${want_build_only}" == 1 ]]; then
  exit 0
fi

if [[ $# -eq 0 ]]; then
  set -- bash
fi

tty_flags=(-i)
if [[ -t 0 && -t 1 ]]; then
  tty_flags=(-it)
fi

exec docker run --rm "${tty_flags[@]}" \
  -u "$(id -u):$(id -g)" \
  --network host \
  -v "${repo_root}:/src" \
  -v "${cache_volume}:/cache" \
  -w /src \
  -e HOME=/cache/home \
  -e GOPATH=/cache/go \
  -e GOCACHE=/cache/go-build \
  -e GOMODCACHE=/cache/go-mod \
  -e GOLANGCI_LINT_CACHE=/cache/golangci-lint \
  -e GOFLAGS=-buildvcs=false \
  -e GOTOOLCHAIN=local \
  -e SCKG_DEV_CONTAINER=1 \
  "${image}" "$@"
