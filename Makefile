# Makefile for KEDA Kaito Scaler

REGISTRY ?= YOUR_REGISTRY
VERSION ?= v0.1.1
IMG_TAG ?= $(subst v,,$(VERSION))
PROJECT_ROOT ?= $(shell pwd)
OUTPUT_DIR := $(PROJECT_ROOT)/_output
LOCALBIN ?= $(PROJECT_ROOT)/bin
$(LOCALBIN):
	mkdir -p $(LOCALBIN)

CONTROLLER_TOOLS_VERSION ?= v0.17.2
CONTROLLER_GEN ?= $(LOCALBIN)/controller-gen

GOLANGCI_LINT_VERSION ?= v1.61.0
GOLANGCI_LINT ?= $(LOCALBIN)/golangci-lint

YQ_VERSION ?= v4.45.1
YQ_TOOL ?= $(LOCALBIN)/yq

# injection variables
INJECTION_ROOT := github.com/kaito-project/keda-kaito-scaler/pkg/injections
BUILD_DATE := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
GIT_COMMIT ?= $(shell git rev-parse --short HEAD)
LDFLAGS := -X '$(INJECTION_ROOT).Version=$(VERSION)' \
		   -X '$(INJECTION_ROOT).BuildDate=$(BUILD_DATE)' \
		   -X '$(INJECTION_ROOT).GitCommit=$(GIT_COMMIT)'


.PHONY: controller-gen
controller-gen: $(CONTROLLER_GEN) ## Download controller-gen locally if necessary. If wrong version is installed, it will be overwritten.
$(CONTROLLER_GEN): $(LOCALBIN)
	test -s $(CONTROLLER_GEN) && $(CONTROLLER_GEN) --version | grep -q $(CONTROLLER_TOOLS_VERSION) || \
	GOBIN=$(LOCALBIN) go install sigs.k8s.io/controller-tools/cmd/controller-gen@$(CONTROLLER_TOOLS_VERSION)

.PHONY: golangci-lint
golangci-lint: $(GOLANGCI_LINT) ## Download golangci-lint locally if necessary. If wrong version is installed, it will be overwritten.
$(GOLANGCI_LINT): $(LOCALBIN)
	test -s $(GOLANGCI_LINT) && $(GOLANGCI_LINT) --version | grep -q $(GOLANGCI_LINT_VERSION) || \
	GOBIN=$(LOCALBIN) go install github.com/golangci/golangci-lint/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)

.PHONY: install-yq
install-yq: $(YQ_TOOL)
$(YQ_TOOL): $(LOCALBIN)
	@if ! command -v $(YQ_TOOL) &> /dev/null; then \
		echo "Installing yq..."; \
		test -s $(YQ_TOOL) || curl -k -L https://github.com/mikefarah/yq/releases/download/${YQ_VERSION}/yq_$(shell go env GOOS)_$(shell go env GOARCH) -o $(YQ_TOOL); \
		chmod +x $(YQ_TOOL); \
	else \
		echo "yq is already installed"; \
	fi

.PHONY: manifests
manifests: controller-gen install-yq ## Generate WebhookConfiguration, ClusterRole and CustomResourceDefinition objects.
	$(CONTROLLER_GEN) rbac:roleName=keda-kaito-scaler-clusterrole paths="./pkg/..." output:rbac:artifacts:config=charts/keda-kaito-scaler/templates
	$(CONTROLLER_GEN) webhook paths="./pkg/..." output:webhook:artifacts:config=charts/keda-kaito-scaler/templates
	mv charts/keda-kaito-scaler/templates/role.yaml charts/keda-kaito-scaler/templates/clusterrole-auto-generated.yaml
	mv charts/keda-kaito-scaler/templates/manifests.yaml charts/keda-kaito-scaler/templates/webhooks-auto-generated.yaml
	$(YQ_TOOL) eval -i 'select(.kind=="MutatingWebhookConfiguration") .metadata.name = "keda-kaito-scaler-mutating-webhook-configuration"' charts/keda-kaito-scaler/templates/webhooks-auto-generated.yaml

.PHONY: generate
generate: controller-gen ## Generate code containing DeepCopy, DeepCopyInto, and DeepCopyObject method implementations.
	$(CONTROLLER_GEN) object:headerFile="hack/boilerplate.go.txt" paths="./..."

.PHONY: verify-mod
verify-mod:
	@echo "verifying go.mod and go.sum"
	go mod tidy
	@if [ -n "$$(git status --porcelain go.mod go.sum)" ]; then \
		echo "Error: go.mod/go.sum is not up-to-date. please run `go mod tidy` and commit the changes."; \
		git diff go.mod go.sum; \
		exit 1; \
	fi

.PHONY: verify-manifests
verify-manifests: manifests
	@echo "verifying manifests"
	@if [ -n "$$(git status --porcelain ./charts)" ]; then \
		echo "Error: manifests are not up-to-date. please run 'make manifests' and commit the changes."; \
		git diff ./charts; \
		exit 1; \
	fi

.PHONY: lint
lint: $(GOLANGCI_LINT)
	$(GOLANGCI_LINT) run -v

.PHONY: build
build: bin/keda-kaito-scaler ## Build the keda-kaito-scaler binary

.PHONY: bin/keda-kaito-scaler
bin/keda-kaito-scaler:
	@mkdir -p $(OUTPUT_DIR)
	CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o $(OUTPUT_DIR)/keda-kaito-scaler ./cmd/main.go

.PHONY: test
test: ## Run tests
	@echo "Running tests..."
	@if find ./pkg ./cmd -name "*_test.go" | grep -q .; then \
		echo "Found test files, running tests with coverage..."; \
		go test -v -race -coverprofile=coverage.out -covermode=atomic ./pkg/... ./cmd/...; \
		go tool cover -html=coverage.out -o coverage.html; \
		echo "Coverage report generated: coverage.html"; \
	else \
		echo "No test files found, creating example to ensure compilation works..."; \
		go test -c ./pkg/... ./cmd/... > /dev/null 2>&1 && echo "✓ Code compiles successfully" || (echo "✗ Compilation failed" && exit 1); \
	fi

## --------------------------------------
## Docker Image Build
## --------------------------------------
BUILDX_BUILDER_NAME ?= img-builder
OUTPUT_TYPE ?= type=registry
QEMU_VERSION ?= 7.2.0-1
ARCH ?= amd64,arm64
BUILDKIT_VERSION ?= v0.18.1

KEDA_KAITO_SCALER_IMG_NAME ?= keda-kaito-scaler

.PHONY: docker-buildx
docker-buildx: ## Build and push docker image for the manager for cross-platform support
	@if ! docker buildx ls | grep $(BUILDX_BUILDER_NAME); then \
		docker run --rm --privileged mcr.microsoft.com/mirror/docker/multiarch/qemu-user-static:$(QEMU_VERSION) --reset -p yes; \
		docker buildx create --name $(BUILDX_BUILDER_NAME) --driver-opt image=mcr.microsoft.com/oss/v2/moby/buildkit:$(BUILDKIT_VERSION) --use; \
		docker buildx inspect $(BUILDX_BUILDER_NAME) --bootstrap; \
	fi

.PHONY: docker-build
docker-build: docker-buildx ## Build and push docker image for the keda-kaito-scaler
	docker buildx build \
		--file ./docker/Dockerfile \
		--output=$(OUTPUT_TYPE) \
		--platform="linux/$(ARCH)" \
		--pull \
		--tag $(REGISTRY)/$(KEDA_KAITO_SCALER_IMG_NAME):$(IMG_TAG) \
		--build-arg VERSION=$(VERSION) \
		--build-arg GIT_COMMIT=$(GIT_COMMIT) \
		--build-arg BUILD_DATE=$(BUILD_DATE) .

.PHONY: docker-build-local
docker-build-local: ## Build docker image locally for testing
	docker build \
		--file ./docker/Dockerfile \
		--tag $(KEDA_KAITO_SCALER_IMG_NAME):$(IMG_TAG) \
		--build-arg VERSION=$(VERSION) \
		--build-arg GIT_COMMIT=$(GIT_COMMIT) \
		--build-arg BUILD_DATE=$(BUILD_DATE) .

.PHONY: clean
clean:
	@echo "Cleaning build artifacts..."
	rm -rf $(OUTPUT_DIR)
	rm -rf $(LOCALBIN)
	rm -f coverage.out

.PHONY: help
help: ## Display this help
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

.PHONY: version
version:
	@echo "Version      : $(VERSION)"
	@echo "Git Commit   : $(GIT_COMMIT)"
	@echo "Build Date   : $(BUILD_DATE)"
