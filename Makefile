# Every target expects the tools of the dev container (hack/devcontainer), so
# run them through it: hack/dev.sh make test

SHELL = /usr/bin/env bash -o pipefail
.SHELLFLAGS = -ec

.PHONY: help
help: ## Display this help.
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2 } ' $(MAKEFILE_LIST)

# The selection API types carry kubebuilder markers; the modules embedding them
# in a CRD run controller-gen themselves. Only the DeepCopy methods are
# generated here.
.PHONY: generate
generate: ## Generate DeepCopy implementations of selection/api.
	controller-gen object:headerFile=hack/boilerplate.go.txt paths="./selection/api/..."

.PHONY: fmt
fmt: ## Run go fmt against code.
	go fmt ./...

.PHONY: vet
vet: ## Run go vet against code.
	go vet ./...

.PHONY: lint
lint: vet ## Run go vet and golangci-lint.
	golangci-lint run

.PHONY: lint-fix
lint-fix: ## Run golangci-lint and apply fixes.
	golangci-lint run --fix

.PHONY: test
test: ## Run the unit tests (offline: they replay keepertest/testdata/snapshot).
	go test ./... -count=1

.PHONY: tidy
tidy: ## go mod tidy.
	go mod tidy

# The snapshot of keepertest/testdata is rebuilt from the query list with the
# public Keeper API (a few dozen requests): make snapshot
.PHONY: snapshot
snapshot: ## Refresh keepertest/testdata/snapshot from the public Keeper API.
	go run ./cmd/sc-snapshot -queries keepertest/testdata/queries.yaml -out keepertest/testdata/snapshot

.PHONY: test-live-keeper
test-live-keeper: ## Every selector against the live Keeper API, all snapshot vendors: accepted queries, push-down without effect on the selection.
	go test -count=1 -tags livekeeper -run TestLiveKeeper -timeout 50m ./selection/
