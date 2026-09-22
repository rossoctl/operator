# Image URL to use all building/pushing image targets
IMG ?= controller:latest

# Get the currently used golang install path (in GOPATH/bin, unless GOBIN is set)
ifeq (,$(shell go env GOBIN))
GOBIN=$(shell go env GOPATH)/bin
else
GOBIN=$(shell go env GOBIN)
endif

# CONTAINER_TOOL defines the container tool to be used for building images.
# Be aware that the target commands are only tested with Docker which is
# scaffolded by default. However, you might want to replace it to use other
# tools. (i.e. podman)
CONTAINER_TOOL ?= docker
CMD_NAME ?= rossoctl-operator
LOCAL_REGISTRY ?= localhost
IMAGE_TAG ?= $(shell git rev-parse --short HEAD)
IMG ?= ${LOCAL_REGISTRY}/${CMD_NAME}:${IMAGE_TAG}
ARCH := $(shell go env GOARCH)
OS := $(shell go env GOOS)

# CLUSTER specifies the Kubernetes cluster name to use for deployment.
# Default is "rossoctl".
# If you need to deploy to a different cluster (e.g., in CI/CD or your local environment),
# override this by running 'make CLUSTER=your-cluster-name ...' or by setting the CLUSTER environment variable.
CLUSTER ?= rossoctl
CONTEXT ?= kind-rossoctl
INTEGRATION_TAGS ?= integration

HELM_IMG := --set controllerManager.container.image.repository=${LOCAL_REGISTRY}/${CMD_NAME} --set controllerManager.container.image.tag=${IMAGE_TAG}
HELM_OPTS ?= ${HELM_IMG}

# Setting SHELL to bash allows bash commands to be executed by recipes.
# Options are set to exit when a recipe line exits non-zero or a piped command fails.
SHELL = /usr/bin/env bash -o pipefail
.SHELLFLAGS = -ec

.PHONY: all
all: build

##@ General

# The help target prints out all targets with their descriptions organized
# beneath their categories. The categories are represented by '##@' and the
# target descriptions by '##'. The awk command is responsible for reading the
# entire set of makefiles included in this invocation, looking for lines of the
# file as xyz: ## something, and then pretty-format the target and help. Then,
# if there's a line with ##@ something, that gets pretty-printed as a category.
# More info on the usage of ANSI control characters for terminal formatting:
# https://en.wikipedia.org/wiki/ANSI_escape_code#SGR_parameters
# More info on the awk command:
# http://linuxcommand.org/lc3_adv_awk.php

.PHONY: help
help: ## Display this help.
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

##@ Development

.PHONY: manifests
manifests: controller-gen ## Generate WebhookConfiguration, ClusterRole and CustomResourceDefinition objects.
	$(CONTROLLER_GEN) rbac:roleName=manager-role crd:allowDangerousTypes=true webhook paths="./..." output:crd:artifacts:config=config/crd/bases
	$(MAKE) sync-chart-crds

# sync-chart-crds keeps charts/operator/crds/ in lockstep with
# config/crd/bases/. Helm's crds/ directory is the install path consumers
# get when they `helm install`; without this sync the chart copy drifts
# silently every time a CRD field is added (which has happened multiple
# times before — commits 677f63d, 21a2258, 4eb62af). Runs as the final
# step of `make manifests` so the chart copy reflects the freshly
# regenerated controller-gen output. Skips the stub `_.yaml` file
# controller-gen sometimes emits.
.PHONY: sync-chart-crds
sync-chart-crds: ## Mirror config/crd/bases/ into charts/operator/crds/.
	@for f in $$(ls config/crd/bases/*.yaml 2>/dev/null); do \
		base=$$(basename $$f); \
		case $$base in \
			_.yaml) ;; \
			*) cp $$f ../charts/operator/crds/$$base ;; \
		esac; \
	done

.PHONY: generate
generate: controller-gen ## Generate code containing DeepCopy, DeepCopyInto, and DeepCopyObject method implementations.
	$(CONTROLLER_GEN) object:headerFile="hack/boilerplate.go.txt" paths="./..."

.PHONY: fmt
fmt: ## Run go fmt against code.
	go fmt ./...

.PHONY: vet
vet: ## Run go vet against code.
	go vet ./...

.PHONY: test
test: manifests generate fmt vet setup-envtest ## Run tests.
	KUBEBUILDER_ASSETS="$(shell $(ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(LOCALBIN) -p path)" go test $$(go list ./... | grep -v /e2e) -coverprofile cover.out

# TODO(user): To use a different vendor for e2e tests, modify the setup under 'tests/e2e'.
# The default setup assumes Kind is pre-installed and builds/loads the Manager Docker image locally.
# Prometheus and CertManager are installed by default; skip with:
# - PROMETHEUS_INSTALL_SKIP=true
# - CERT_MANAGER_INSTALL_SKIP=true
.PHONY: test-e2e
test-e2e: manifests generate fmt vet ## Run the e2e tests. Expected an isolated environment using Kind.
	@command -v kind >/dev/null 2>&1 || { \
		echo "Kind is not installed. Please install Kind manually."; \
		exit 1; \
	}
	@kind get clusters | grep -q 'kind' || { \
		echo "No Kind cluster is running. Please start a Kind cluster before running the e2e tests."; \
		exit 1; \
	}
	go test ./test/e2e/ -v -ginkgo.v -timeout 45m

.PHONY: test-integration
test-integration: manifests generate fmt vet ## Run integration tests (requires Kind cluster)
	@command -v kind >/dev/null 2>&1 || { \
		echo "ERROR: kind is not installed. Install from https://kind.sigs.k8s.io/"; \
		exit 1; \
	}
	@kind get clusters | grep -q 'kind' || { \
		echo "ERROR: no Kind cluster found. Create one with: kind create cluster"; \
		exit 1; \
	}
	$(MAKE) install
	go test -v -tags=$(INTEGRATION_TAGS) ./test/integration/... -timeout 5m

.PHONY: lint
lint: golangci-lint ## Run golangci-lint linter
	$(GOLANGCI_LINT) run

.PHONY: lint-fix
lint-fix: golangci-lint ## Run golangci-lint linter and perform fixes
	$(GOLANGCI_LINT) run --fix

.PHONY: lint-config
lint-config: golangci-lint ## Verify golangci-lint linter configuration
	$(GOLANGCI_LINT) config verify

##@ Build

.PHONY: build
build: manifests generate fmt vet ## Build manager, bundle-service and token-broker binaries.
	# All three ship in the operator image (see Dockerfile), so all three are
	# built here: a break confined to one of the secondary main packages would
	# otherwise pass CI and only surface during the image build.
	go build -o bin/manager cmd/main.go
	go build -o bin/bundle-service ./cmd/bundle-service/
	go build -o bin/token-broker ./cmd/token-broker/

# Self-signed TLS for the validating webhook when running outside the cluster (controller-runtime
# otherwise expects tls.crt/tls.key under $TMPDIR/k8s-webhook-server/serving-certs).
WEBHOOK_CERT_DIR ?= $(CURDIR)/.run/webhook-certs

.PHONY: run
run: manifests generate fmt vet ## Run a controller from your host (needs cluster + CRDs: make install).
	@mkdir -p "$(WEBHOOK_CERT_DIR)"
	@if [ ! -f "$(WEBHOOK_CERT_DIR)/tls.crt" ] || [ ! -f "$(WEBHOOK_CERT_DIR)/tls.key" ]; then \
		command -v openssl >/dev/null 2>&1 || { \
			echo "openssl not found: install it, or create tls.crt/tls.key under WEBHOOK_CERT_DIR and re-run"; \
			echo "  WEBHOOK_CERT_DIR=$(WEBHOOK_CERT_DIR)"; \
			exit 1; \
		}; \
		openssl req -x509 -newkey rsa:2048 -nodes \
			-keyout "$(WEBHOOK_CERT_DIR)/tls.key" \
			-out "$(WEBHOOK_CERT_DIR)/tls.crt" \
			-days 365 \
			-subj "/CN=localhost"; \
	fi
	go run ./cmd/main.go \
		--webhook-cert-path="$(WEBHOOK_CERT_DIR)" \
		--metrics-secure=false

# If you wish to build the manager image targeting other platforms you can use the --platform flag.
# (i.e. docker build --platform linux/arm64). However, you must enable docker buildKit for it.
# More info: https://docs.docker.com/develop/develop-images/build_enhancements/
.PHONY: docker-build
docker-build: ## Build docker image with the manager.
	$(CONTAINER_TOOL) build -t ${IMG} .

.PHONY: docker-push
docker-push: ## Push docker image with the manager.
	$(CONTAINER_TOOL) push ${IMG}

# PLATFORMS defines the target platforms for the manager image be built to provide support to multiple
# architectures. (i.e. make docker-buildx IMG=myregistry/mypoperator:0.0.1). To use this option you need to:
# - be able to use docker buildx. More info: https://docs.docker.com/build/buildx/
# - have enabled BuildKit. More info: https://docs.docker.com/develop/develop-images/build_enhancements/
# - be able to push the image to your registry (i.e. if you do not set a valid value via IMG=<myregistry/image:<tag>> then the export will fail)
# To adequately provide solutions that are compatible with multiple platforms, you should consider using this option.
PLATFORMS ?= linux/arm64,linux/amd64,linux/s390x,linux/ppc64le
.PHONY: docker-buildx
docker-buildx: ## Build and push docker image for the manager for cross-platform support
	- $(CONTAINER_TOOL) buildx create --name rossoctl-operator-builder
	$(CONTAINER_TOOL) buildx use rossoctl-operator-builder
	- $(CONTAINER_TOOL) buildx build --push --platform=$(PLATFORMS) --tag ${IMG} .
	- $(CONTAINER_TOOL) buildx rm rossoctl-operator-builder

.PHONY: manifests-openshift
manifests-openshift: manifests kustomize ## Build OpenShift standalone kustomize overlay manifests.
	$(KUSTOMIZE) build config/openshift

.PHONY: build-installer
build-installer: manifests generate kustomize ## Generate a consolidated YAML with CRDs and deployment.
	mkdir -p dist
	cd config/manager && $(KUSTOMIZE) edit set image controller=${IMG}
	$(KUSTOMIZE) build config/default > dist/install.yaml

SIGNER_IMG ?= ghcr.io/rossoctl/operator/agentcard-signer:latest
KIND_CLUSTER_NAME ?= $(CLUSTER)

.PHONY: build-signer
build-signer: ## Build the agentcard-signer init-container image.
	$(CONTAINER_TOOL) build -t $(SIGNER_IMG) -f cmd/agentcard-signer/Dockerfile .

.PHONY: load-signer-image
load-signer-image: ## Load the agentcard-signer image into Kind.
	kind load docker-image $(SIGNER_IMG) --name $(KIND_CLUSTER_NAME)

##@ Deployment

ifndef ignore-not-found
  ignore-not-found = false
endif

.PHONY: install
install: manifests kustomize ## Install CRDs into the K8s cluster specified in ~/.kube/config.
	$(KUSTOMIZE) build config/crd | $(KUBECTL) apply --server-side  -f -

.PHONY: uninstall
uninstall: manifests kustomize ## Uninstall CRDs from the K8s cluster specified in ~/.kube/config. Call with ignore-not-found=true to ignore resource not found errors during deletion.
	$(KUSTOMIZE) build config/crd | $(KUBECTL) delete --ignore-not-found=$(ignore-not-found) -f -

.PHONY: deploy
deploy: manifests kustomize ## Deploy controller to the K8s cluster specified in ~/.kube/config.
	cd config/manager && $(KUSTOMIZE) edit set image controller=${IMG}
	$(KUSTOMIZE) build config/default | $(KUBECTL) apply --server-side  -f -

.PHONY: deploy-openshift
deploy-openshift: manifests kustomize ## Deploy controller with OpenShift extras (istio-trust, authbridge, SCC).
	cd config/manager && $(KUSTOMIZE) edit set image controller=${IMG}
	$(KUSTOMIZE) build config/openshift | $(KUBECTL) apply --server-side  -f -

.PHONY: undeploy
undeploy: kustomize ## Undeploy controller from the K8s cluster specified in ~/.kube/config. Call with ignore-not-found=true to ignore resource not found errors during deletion.
	$(KUSTOMIZE) build config/default | $(KUBECTL) delete --ignore-not-found=$(ignore-not-found) -f -

.PHONY: undeploy-openshift
undeploy-openshift: kustomize ## Undeploy controller with OpenShift extras. Call with ignore-not-found=true to ignore resource not found errors.
	$(KUSTOMIZE) build config/openshift | $(KUBECTL) delete --ignore-not-found=$(ignore-not-found) -f -

.PHONY: chart
chart: manifests kustomize
	cd config/manager && $(KUSTOMIZE) edit set image controller=$(shell echo ${IMG} | sed 's/\(:.*\)v/\1/')
	$(KUSTOMIZE) build config/default > ../charts/operator/templates/operator.yaml
	$(KUSTOMIZE) build config/crd > ../charts/operator/templates/crd/crds.yaml

# this is used for local testing
.PHONY: kind-load-image
kind-load-image:
	kind load docker-image ${LOCAL_REGISTRY}/${CMD_NAME}:${IMAGE_TAG} --name ${CLUSTER}

.PHONY: install-local-chart
install-local-chart: kind-load-image
	if [ -z "$(CONTEXT)" ] ; then helm upgrade --install --create-namespace -n rossoctl-system rossoctl-operator ../charts/operator ${HELM_OPTS} ; else helm  --kube-context ${CONTEXT} upgrade --install --create-namespace -n rossoctl-system rossoctl-operator ../charts/operator ${HELM_OPTS} ; fi
##@ Dependencies

## Location to install dependencies to
LOCALBIN ?= $(shell pwd)/bin
$(LOCALBIN):
	mkdir -p $(LOCALBIN)

## Tool Binaries
KUBECTL ?= kubectl
KUSTOMIZE ?= $(LOCALBIN)/kustomize
CONTROLLER_GEN ?= $(LOCALBIN)/controller-gen
ENVTEST ?= $(LOCALBIN)/setup-envtest
GOLANGCI_LINT = $(LOCALBIN)/golangci-lint

## Tool Versions
KUSTOMIZE_VERSION ?= v5.5.0
CONTROLLER_TOOLS_VERSION ?= v0.17.1
#ENVTEST_VERSION is the version of controller-runtime release branch to fetch the envtest setup script (i.e. release-0.20)
ENVTEST_VERSION ?= $(shell go list -m -f "{{ .Version }}" sigs.k8s.io/controller-runtime | awk -F'[v.]' '{printf "release-%d.%d", $$2, $$3}')
#ENVTEST_K8S_VERSION is the version of Kubernetes to use for setting up ENVTEST binaries (i.e. 1.31)
ENVTEST_K8S_VERSION ?= $(shell go list -m -f "{{ .Version }}" k8s.io/api | awk -F'[v.]' '{printf "1.%d", $$3}')
GOLANGCI_LINT_VERSION ?= v1.63.4

.PHONY: kustomize
kustomize: $(KUSTOMIZE) ## Download kustomize locally if necessary.
$(KUSTOMIZE): $(LOCALBIN)
	$(call go-install-tool,$(KUSTOMIZE),sigs.k8s.io/kustomize/kustomize/v5,$(KUSTOMIZE_VERSION))

.PHONY: controller-gen
controller-gen: $(CONTROLLER_GEN) ## Download controller-gen locally if necessary.
$(CONTROLLER_GEN): $(LOCALBIN)
	$(call go-install-tool,$(CONTROLLER_GEN),sigs.k8s.io/controller-tools/cmd/controller-gen,$(CONTROLLER_TOOLS_VERSION))

.PHONY: setup-envtest
setup-envtest: envtest ## Download the binaries required for ENVTEST in the local bin directory.
	@echo "Setting up envtest binaries for Kubernetes version $(ENVTEST_K8S_VERSION)..."
	@$(ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(LOCALBIN) -p path || { \
		echo "Error: Failed to set up envtest binaries for version $(ENVTEST_K8S_VERSION)."; \
		exit 1; \
	}

.PHONY: envtest
envtest: $(ENVTEST) ## Download setup-envtest locally if necessary.
$(ENVTEST): $(LOCALBIN)
	$(call go-install-tool,$(ENVTEST),sigs.k8s.io/controller-runtime/tools/setup-envtest,$(ENVTEST_VERSION))

.PHONY: golangci-lint
golangci-lint: $(GOLANGCI_LINT) ## Download golangci-lint locally if necessary.
$(GOLANGCI_LINT): $(LOCALBIN)
	$(call go-install-tool,$(GOLANGCI_LINT),github.com/golangci/golangci-lint/cmd/golangci-lint,$(GOLANGCI_LINT_VERSION))

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
