# Copyright The Kubernetes Authors
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#    https://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

DOCKER   ?= docker
MKDIR    ?= mkdir
TR       ?= tr
CC       ?= cc
DIST_DIR ?= $(CURDIR)/dist

include $(CURDIR)/common.mk
include $(CURDIR)/versions.mk

.NOTPARALLEL:

ifeq ($(IMAGE_NAME),)
IMAGE_NAME = $(REGISTRY)/$(DRIVER_NAME)
endif

CMDS := $(patsubst ./cmd/%/,%,$(sort $(dir $(wildcard ./cmd/*/))))
CMD_TARGETS := $(patsubst %,cmd-%, $(CMDS))

CHECK_TARGETS := golangci-lint check-generate

MAKE_TARGETS := binaries build build-image check clean fmt lint-internal test examples cmds coverage generate vendor check-modules helm-lint helm-test helm-package $(CHECK_TARGETS)

TARGETS := $(MAKE_TARGETS) $(CMD_TARGETS)

DOCKER_TARGETS := $(patsubst %,docker-%, $(TARGETS))
.PHONY: $(TARGETS) $(DOCKER_TARGETS)

GOOS ?= linux
GOARCH ?= $(shell uname -m | sed -e 's,aarch64,arm64,' -e 's,x86_64,amd64,')
ifeq ($(VERSION),)
CLI_VERSION = $(LIB_VERSION)$(if $(LIB_TAG),-$(LIB_TAG))
else
CLI_VERSION = $(VERSION)
endif
CLI_VERSION_PACKAGE = $(MODULE)/internal/info

binaries: cmds
ifneq ($(PREFIX),)
cmd-%: COMMAND_BUILD_OPTIONS = -o $(PREFIX)/$(*)
endif
cmds: $(CMD_TARGETS)
$(CMD_TARGETS): cmd-%:
	CGO_LDFLAGS_ALLOW='-Wl,--unresolved-symbols=ignore-in-object-files' \
		CC=$(CC) CGO_ENABLED=1 GOOS=$(GOOS) GOARCH=$(GOARCH) \
		go build -ldflags "-s -w -X $(CLI_VERSION_PACKAGE).gitCommit=$(GIT_COMMIT) -X $(CLI_VERSION_PACKAGE).version=$(CLI_VERSION)" $(COMMAND_BUILD_OPTIONS) $(MODULE)/cmd/$(*)

build:
	CC=$(CC) GOOS=$(GOOS) GOARCH=$(GOARCH) go build ./...

clean:
	@for cmd in $(CMDS); do \
		rm -f $(CURDIR)/$$cmd; \
		[ -n "$(PREFIX)" ] && rm -f $(PREFIX)/$$cmd || true; \
	done

examples: $(EXAMPLE_TARGETS)
$(EXAMPLE_TARGETS): example-%:
	CC=$(CC) GOOS=$(GOOS) GOARCH=$(GOARCH) go build ./examples/$(*)

all: check test build binaries
check: $(CHECK_TARGETS)

# Apply go fmt to the codebase
fmt:
	go list -f '{{.Dir}}' $(MODULE)/... \
		| xargs gofmt -s -l -w

## goimports: Apply goimports -local to the codebase
goimports:
	find . -name \*.go \
			-not -name "zz_generated.deepcopy.go" \
			-not -path "./vendor/*" \
			-not -path "./$(PKG_BASE)/clientset/versioned/*" \
		-exec goimports -local $(MODULE) -w {} \;

golangci-lint:
	golangci-lint run ./...

lint-internal: golangci-lint

helm-lint: helm-test
	helm lint deployments/helm/dra-driver-nvidia-gpu

helm-test:
	hack/test-helm-policy-service-account.sh

helm-package:
	helm package deployments/helm/dra-driver-nvidia-gpu

vendor:
	go mod tidy
	go mod vendor
	go mod verify

check-modules: vendor
	git diff --quiet HEAD -- go.mod go.sum vendor

COVERAGE_FILE := coverage.out
test: build cmds
	go test -race -cover -v -coverprofile=$(COVERAGE_FILE) \
		-ldflags "-X $(CLI_VERSION_PACKAGE).version=$(CLI_VERSION)" \
		$(MODULE)/...

coverage: test
	cat $(COVERAGE_FILE) | grep -v "_mock.go" > $(COVERAGE_FILE).no-mocks
	go tool cover -func=$(COVERAGE_FILE).no-mocks

generate: generate-crds generate-informers fmt

generate-crds: generate-deepcopy .remove-crds
	for dir in $(CLIENT_SOURCES); do \
		controller-gen crd:crdVersions=v1 \
			paths=$(CURDIR)/$${dir} \
			output:crd:dir=$(CURDIR)/deployments/helm/tmp_crds; \
	done
	mkdir -p $(CURDIR)/deployments/helm/$(DRIVER_NAME)/crds
	cp -R $(CURDIR)/deployments/helm/tmp_crds/* \
		$(CURDIR)/deployments/helm/$(DRIVER_NAME)/crds
	rm -rf $(CURDIR)/deployments/helm/tmp_crds


# Regenerate everything and fail if the tree is dirty (used by `make check`).
check-generate: generate
	git diff --exit-code HEAD

generate-deepcopy: .remove-deepcopy
	for dir in $(DEEPCOPY_SOURCES); do \
		controller-gen \
			object:headerFile=$(CURDIR)/hack/boilerplate.go.txt,year=$(shell date +"%Y") \
			paths=$(CURDIR)/$${dir}/ \
			output:object:dir=$(CURDIR)/$${dir}; \
	done

generate-informers: .remove-informers generate-listers
	informer-gen \
		--go-header-file=$(CURDIR)/hack/boilerplate.go.txt \
		--output-package "$(MODULE)/$(PKG_BASE)/informers" \
        --input-dirs "$(shell for api in $(CLIENT_APIS); do printf '%s' "$(MODULE)/$(API_BASE)/$$api,"; done | sed 's/,$$//')" \
		--output-base "$(CURDIR)/pkg/tmp_informers" \
		--versioned-clientset-package "$(MODULE)/$(PKG_BASE)/clientset/versioned" \
		--listers-package "$(MODULE)/$(PKG_BASE)/listers"
	mkdir -p $(CURDIR)/$(PKG_BASE)
	mv $(CURDIR)/pkg/tmp_informers/$(MODULE)/$(PKG_BASE)/informers \
	   $(CURDIR)/$(PKG_BASE)/informers
	rm -rf $(CURDIR)/pkg/tmp_informers

generate-listers: .remove-listers generate-clientset
	lister-gen \
		--go-header-file=$(CURDIR)/hack/boilerplate.go.txt \
		--output-package "$(MODULE)/$(PKG_BASE)/listers" \
        --input-dirs "$(shell for api in $(CLIENT_APIS); do printf '%s' "$(MODULE)/$(API_BASE)/$$api,"; done | sed 's/,$$//')" \
		--output-base "$(CURDIR)/pkg/tmp_listers"
	mkdir -p $(CURDIR)/$(PKG_BASE)
	mv $(CURDIR)/pkg/tmp_listers/$(MODULE)/$(PKG_BASE)/listers \
	   $(CURDIR)/$(PKG_BASE)/listers
	rm -rf $(CURDIR)/pkg/tmp_listers

generate-clientset: .remove-clientset
	client-gen \
		--go-header-file=$(CURDIR)/hack/boilerplate.go.txt \
		--clientset-name "versioned" \
		--build-tag "ignore_autogenerated" \
		--output-package "$(MODULE)/$(PKG_BASE)/clientset" \
		--input-base "$(MODULE)/$(API_BASE)" \
		--output-base "$(CURDIR)/pkg/tmp_clientset" \
		--input "$(shell echo $(CLIENT_APIS) | tr ' ' ',')" \
		--plural-exceptions "$(shell echo $(PLURAL_EXCEPTIONS) | tr ' ' ',')"
	mkdir -p $(CURDIR)/$(PKG_BASE)
	mv $(CURDIR)/pkg/tmp_clientset/$(MODULE)/$(PKG_BASE)/clientset \
       $(CURDIR)/$(PKG_BASE)/clientset
	rm -rf $(CURDIR)/pkg/tmp_clientset

.remove-crds:
	rm -rf $(CURDIR)/deployments/helm/$(DRIVER_NAME)/crds

.remove-deepcopy:
	for dir in $(DEEPCOPY_SOURCES); do \
		rm -f $(CURDIR)/$${dir}/zz_generated.deepcopy.go; \
	done

.remove-clientset:
	rm -rf $(CURDIR)/$(PKG_BASE)/clientset

.remove-listers:
	rm -rf $(CURDIR)/$(PKG_BASE)/listers

.remove-informers:
	rm -rf $(CURDIR)/$(PKG_BASE)/informers

# Generate an image for containerized builds
# Note: This image is local only
.PHONY: .build-image build-image
build-image: .build-image
.build-image:
	make -f deployments/devel/Makefile .build-image

ifeq ($(BUILD_DEVEL_IMAGE),yes)
$(DOCKER_TARGETS): .build-image
.shell: .build-image
endif

$(DOCKER_TARGETS): docker-%:
	@echo "Running 'make $(*)' in container image $(BUILDIMAGE)"
	$(DOCKER) run \
		--rm \
		-e GOCACHE=/tmp/.cache/go \
		-e GOMODCACHE=/tmp/.cache/gomod \
		-e GOLANGCI_LINT_CACHE=/tmp/.cache/golangci-lint \
		-v $(PWD):/work \
		-w /work \
		--user $$(id -u):$$(id -g) \
		$(BUILDIMAGE) \
			make $(*)

# Start an interactive shell using the development image.
PHONY: .shell
.shell:
	$(DOCKER) run \
		--rm \
		-ti \
		-e GOCACHE=/tmp/.cache/go \
		-e GOMODCACHE=/tmp/.cache/gomod \
		-e GOLANGCI_LINT_CACHE=/tmp/.cache/golangci-lint \
		-v $(PWD):/work \
		-w /work \
		--user $$(id -u):$$(id -g) \
		$(BUILDIMAGE)

.PHONY: bats bats-cd bats-gpu
bats:
	make -f tests/bats/Makefile tests

# Run compute domain specific tests
bats-cd:
	make -f tests/bats/Makefile tests-cd

# Run GPU plugin specific tests
bats-gpu:
	make -f tests/bats/Makefile tests-gpu

.PHONY: image-build-and-copy-to-nodes
image-build-and-copy-to-nodes:
	make -f deployments/container/Makefile build
	bash hack/copy-image-to-k8s-nodes.sh $(REGISTRY)/$(DRIVER_NAME):$(VERSION)

# GCP + nvkind + GPU Operator e2e harness.
# For local dev: set GCP_PROJECT=<your-project> and (optionally) GCE_ZONE,
# which short-circuits Boskos. See hack/ci/gcp-nvkind/README.md.
.PHONY: e2e-gcp-nvkind
e2e-gcp-nvkind:
	bash hack/ci/gcp-nvkind/e2e-test.sh

# Run the Go/Ginkgo e2e suite against the current kubectl context. Assumes
# the cluster already has GPU Operator (minimal mode) + the DRA driver
# installed. The -tags=e2e build tag keeps this out of `make test`.
.PHONY: test-e2e
test-e2e:
	ARTIFACTS=$${ARTIFACTS:-/tmp/test-e2e-artifacts}; mkdir -p $$ARTIFACTS; \
	go test -mod=vendor -tags=e2e -v -timeout=30m ./test/e2e/... \
	  -ginkgo.v -ginkgo.junit-report=$$ARTIFACTS/junit_01.xml
