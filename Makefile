IMAGE_REGISTRY ?= quay.io/opendatahub
IMAGE_NAME ?= mcp-lifecycle-module-operator
IMAGE_TAG ?= latest
IMG ?= $(IMAGE_REGISTRY)/$(IMAGE_NAME):$(IMAGE_TAG)
PLATFORM ?= linux/amd64
CGO_ENABLED ?= 1
COMMON_BUILD_ARGS += -trimpath -ldflags="-s -w"
OUTPUT ?= ./bin/manager
CLEAN_TARGETS ?= $(OUTPUT)

MCPLO_REPO ?= https://github.com/opendatahub-io/mcp-lifecycle-operator
MCPLO_REF ?= main

CONTAINER_TOOL ?= docker

ifeq (,$(shell go env GOBIN))
GOBIN=$(shell go env GOPATH)/bin
else
GOBIN=$(shell go env GOBIN)
endif

SHELL = /usr/bin/env bash -o pipefail
.SHELLFLAGS = -ec

.PHONY: all
all: build

##@ General

.PHONY: help
help: ## Display this help.
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", $$2 }' $(MAKEFILE_LIST)

##@ Development
.PHONY: clean
clean: ## Clean up all build artifacts
	rm -rf $(CLEAN_TARGETS)

.PHONY: manifests
manifests: controller-gen ## Generate ClusterRole and CustomResourceDefinition objects.
	$(CONTROLLER_GEN) rbac:roleName=manager-role crd paths="./..." output:crd:artifacts:config=config/crd/bases output:rbac:artifacts:config=config/rbac

.PHONY: generate
generate: controller-gen ## Generate code containing DeepCopy implementations.
	$(CONTROLLER_GEN) object:headerFile="hack/boilerplate.go.txt" paths="./..."

.PHONY: fmt
fmt: ## Run go fmt against code.
	go fmt ./...

.PHONY: vet
vet: ## Run go vet against code.
	go vet ./...

.PHONY: verify
verify: manifests generate fmt ## Verify generated code and formatting are up-to-date.
	@if [ -n "$$(git status --porcelain)" ]; then \
		echo "ERROR: generated files are out of date. Run 'make manifests generate fmt' and commit the result."; \
		git status --porcelain; \
		git diff; \
		exit 1; \
	else \
		echo "Generated code and formatting are up-to-date."; \
	fi

.PHONY: test
test: manifests generate fmt vet ## Run tests with coverage.
	go test ./... -coverprofile cover.out

.PHONY: unit-test
unit-test: ## Run unit tests (no codegen prerequisites).
	go test ./internal/... ./cmd/...

.PHONY: kind-create
kind-create: ## Create a Kind cluster with a local registry.
	hack/create-kind-cluster.sh

.PHONY: kind-delete
kind-delete: ## Delete the Kind cluster.
	kind delete cluster

.PHONY: e2e-test
e2e-test: ## Run E2E tests (requires a deployed operator on a running cluster).
	go test ./test/e2e/ -v -timeout 15m

##@ Build

.PHONY: build
build: clean fmt ## Build manager binary.
	mkdir -p $(dir $(OUTPUT))
	CGO_ENABLED=$(CGO_ENABLED) $(GO_BUILD_ENV) go build $(COMMON_BUILD_ARGS) -tags=strictfipsruntime -mod=vendor -a -o $(OUTPUT) cmd/main.go

.PHONY: run
run: manifests generate fmt vet ## Run a controller from your host.
	go run ./cmd/main.go

.PHONY: docker-build
docker-build: ## Build docker image with the manager.
	$(CONTAINER_TOOL) build --platform $(PLATFORM) --build-arg CGO_ENABLED=$(CGO_ENABLED) -f Dockerfile -t ${IMG} .

.PHONY: docker-push
docker-push: ## Push docker image with the manager.
	$(CONTAINER_TOOL) push ${IMG}

##@ Deployment

ifndef ignore-not-found
  ignore-not-found = false
endif

.PHONY: install
install: manifests kustomize ## Install CRDs into the K8s cluster specified in ~/.kube/config.
	$(KUSTOMIZE) build config/crd | kubectl apply -f -

.PHONY: uninstall
uninstall: manifests kustomize ## Uninstall CRDs from the K8s cluster.
	$(KUSTOMIZE) build config/crd | kubectl delete --ignore-not-found=$(ignore-not-found) -f -

.PHONY: deploy
deploy: manifests kustomize ## Deploy controller to the K8s cluster.
	cd config/manager && $(KUSTOMIZE) edit set image controller=${IMG}
	$(KUSTOMIZE) build config/default | kubectl apply -f -

.PHONY: undeploy
undeploy: ## Undeploy controller from the K8s cluster.
	$(KUSTOMIZE) build config/default | kubectl delete --ignore-not-found=$(ignore-not-found) -f -

##@ Operand Manifests

.PHONY: update-operand-manifests
update-operand-manifests: kustomize ## Vendor MCPLO manifests from the midstream fork.
	$(eval TMP := $(shell mktemp -d))
	git clone --depth 1 --branch "$(MCPLO_REF)" "$(MCPLO_REPO)" "$(TMP)"
	"$(KUSTOMIZE)" build "$(TMP)/config/default" > internal/controller/resources/mcp-lifecycle-operator.yaml
	rm -rf "$(TMP)"

##@ Build Dependencies

LOCALBIN ?= $(shell pwd)/bin
$(LOCALBIN):
	mkdir -p $(LOCALBIN)

## Tool Binaries
KUSTOMIZE ?= $(LOCALBIN)/kustomize
CONTROLLER_GEN ?= $(LOCALBIN)/controller-gen

## Tool Versions
KUSTOMIZE_VERSION ?= v5.8.1
CONTROLLER_TOOLS_VERSION ?= v0.21.0

.PHONY: kustomize
kustomize: $(KUSTOMIZE) ## Download kustomize locally if necessary.
$(KUSTOMIZE): $(LOCALBIN)
	$(call go-install-tool,$(KUSTOMIZE),sigs.k8s.io/kustomize/kustomize/v5,$(KUSTOMIZE_VERSION))

.PHONY: controller-gen
controller-gen: $(CONTROLLER_GEN) ## Download controller-gen locally if necessary.
$(CONTROLLER_GEN): $(LOCALBIN)
	$(call go-install-tool,$(CONTROLLER_GEN),sigs.k8s.io/controller-tools/cmd/controller-gen,$(CONTROLLER_TOOLS_VERSION))

# go-install-tool will 'go install' any package with custom target and name of binary, if it doesn't exist
# $1 - target path with name of binary
# $2 - package url which can be installed
# $3 - specific version of package
define go-install-tool
@[ -f "$(1)-$(3)" ] || { \
set -e; \
package=$(2)@$(3) ;\
echo "Downloading $${package}" ;\
rm -f $(1) || true ;\
GOBIN=$(LOCALBIN) go install $${package} ;\
mv $(1) $(1)-$(3) ;\
} ;\
ln -sf $(1)-$(3) $(1)
endef