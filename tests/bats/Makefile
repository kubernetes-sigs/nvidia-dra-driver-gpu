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

include $(CURDIR)/versions.mk
include $(CURDIR)/common.mk

# The to-be-tested Helm chart. Ignored when setting TEST_CHART_LOCAL.
TEST_CHART_REPO ?= "oci://gcr.io/k8s-staging-nvidia/charts/dra-driver-nvidia-gpu"
TEST_CHART_VERSION ?= "$(VERSION_STAGING_CHART)"

# The baseline Helm chart to test upgrades from and downgrades to.
TEST_CHART_LASTSTABLE_REPO ?= "oci://registry.k8s.io/dra-driver-nvidia/charts/dra-driver-nvidia-gpu"
TEST_CHART_LASTSTABLE_VERSION ?= "0.4.0"

# If not "false": the to-be-tested Helm chart is installed from the local
# filesystem (from `deployments/helm/dra-driver-nvidia-gpu`). Make sure
# (out-of-band) that the container images that the chart refers to are available
# (placed directly on the involved nodes or pullable). This is a convenince
# parameter for setting
#
#        TEST_CHART_REPO="deployments/helm/dra-driver-nvidia-gpu/"
#        TEST_CHART_VERSION="$(make print-VERSION)" (but w/o v prefix)
#
TEST_CHART_LOCAL ?= "false"

# Consumed in upgrade test via kubectl apply -f <URL>
# (can be a branch, tag, or commit). TODO: parse default
# from `TEST_CHART_VERSION`.
TEST_CRD_UPGRADE_TARGET_GIT_REF ?= "main"

TEST_NVIDIA_DRIVER_ROOT ?= "/run/nvidia/driver"

# TODO: calculate precise, expected container image spec,
# and test for that in test suite, for example:
# `registry.k8s.io/dra-driver-nvidia/dra-driver-nvidia-gpu:v25.8.0-dev`
TEST_EXPECTED_IMAGE_SPEC_SUBSTRING ?= $(VERSION)

ifneq ($(TEST_CHART_LOCAL),"false")
TEST_CHART_REPO = "deployments/helm/dra-driver-nvidia-gpu"
TEST_CHART_VERSION = $(VERSION:v%=%)
endif

# Note: at times, it may be valuable to use
# 	--show-output-of-passing-tests
# in the argument list below.
BATS_IMAGE ?= batstests:$(GIT_COMMIT_SHORT)
BATS_ARGS ?= \
	--print-output-on-failure \
	--no-tempdir-cleanup \
	--timing \
	--abort

# Evaluates to`true` only when TEST_FILTER_TAGS is non-empty after stripping
# whitespace.
ifneq ($(strip $(TEST_FILTER_TAGS)),)
	BATS_ARGS += --filter-tags $(TEST_FILTER_TAGS)
endif

ifneq ($(strip $(TEST_ALWAYS_SHOW_OUTPUT)),)
	BATS_ARGS += --show-output-of-passing-tests
endif

KUBECONFIG ?= $(HOME)/.kube/config

TMPDIR ?= "/tmp"
# Add `docker run` arguments when not running in Github Actions / GitLab CI. Set
# TMPDIR to checkout directory (easier for runner setup).
DOCKER_RUN_FLAGS :=
ifeq ($(CI),)
  DOCKER_RUN_FLAGS += -it
  TEST_GPU_STRESS_PODS_N = 12
  TEST_GPU_STRESS_ITERATIONS = 2
  TEST_GPU_STRESS_DELAY = 1
else
  TMPDIR=$(CURDIR)
  TEST_GPU_STRESS_PODS_N = 20
  TEST_GPU_STRESS_ITERATIONS = 1
  TEST_GPU_STRESS_DELAY = 1
endif

# DOCKER_NETWORK overrides the Docker network mode for the BATS runner container.
# Set to "host" for Kind-based testing where the kubeconfig uses 127.0.0.1.
ifneq ($(DOCKER_NETWORK),)
  DOCKER_RUN_FLAGS += --network $(DOCKER_NETWORK)
endif

RUNDIR_PARENT ?= $(TMPDIR)/k8s-dra-driver-gpu-tests-out-$(USER)

DOCKER_ENVS = \
	--env KUBECONFIG=/kubeconfig \
	--env USER=$(USER) \
	--env TEST_CHART_REPO=$(TEST_CHART_REPO) \
	--env TEST_CHART_VERSION=$(TEST_CHART_VERSION) \
	--env TEST_CHART_LASTSTABLE_REPO=$(TEST_CHART_LASTSTABLE_REPO) \
	--env TEST_CHART_LASTSTABLE_VERSION=$(TEST_CHART_LASTSTABLE_VERSION) \
	--env TEST_GPU_STRESS_PODS_N=$(TEST_GPU_STRESS_PODS_N) \
	--env TEST_GPU_STRESS_ITERATIONS=$(TEST_GPU_STRESS_ITERATIONS) \
	--env TEST_GPU_STRESS_DELAY=$(TEST_GPU_STRESS_DELAY) \
	--env TEST_CRD_UPGRADE_TARGET_GIT_REF=$(TEST_CRD_UPGRADE_TARGET_GIT_REF) \
	--env TEST_NVIDIA_DRIVER_ROOT=$(TEST_NVIDIA_DRIVER_ROOT) \
	--env TEST_EXPECTED_IMAGE_SPEC_SUBSTRING=$(TEST_EXPECTED_IMAGE_SPEC_SUBSTRING) \
	--env SKIP_CLEANUP=$(SKIP_CLEANUP) \
	--env DISABLE_COMPUTE_DOMAINS=$(DISABLE_COMPUTE_DOMAINS) \
	--env TEST_DRA_LIST_TYPE_ATTRIBUTES=$(TEST_DRA_LIST_TYPE_ATTRIBUTES) \
	--env NVMM_PATH=$(NVMM_PATH) \
	--env TEST_ALT_PROC_DEVICES=$(TEST_ALT_PROC_DEVICES) \
	--env MOCK_NVML=$(MOCK_NVML)

DOCKER_UID := $(shell id -u)
DOCKER_GID := $(shell id -g)
DOCKER_USER := $(DOCKER_UID):$(DOCKER_GID)

DOCKER_MOUNTS = \
	-v /tmp:/tmp \
	-v $(CURDIR):/cwd \
	-v $(KUBECONFIG):/kubeconfig

# Helper to run bats inside container
# $(1) = whitespace-separated list of test files
define RUN_BATS
	@echo "KUBECONFIG: $(KUBECONFIG)" && \
	echo "BATS_ARGS: $(BATS_ARGS) " && \
	echo "TMPDIR: $(TMPDIR)" && \
	export _RUNDIR=$(RUNDIR_PARENT)/bats-tests-$$(date +%s) && \
	mkdir -p "$${_RUNDIR}" && \
	echo "RUNDIR: $${_RUNDIR}" &&  \
	docker run --rm $(DOCKER_RUN_FLAGS) $(DOCKER_MOUNTS) $(DOCKER_ENVS) \
	  -u $(DOCKER_USER) --entrypoint /bin/bash $(BATS_IMAGE) \
	  -c "set -ex; cd /cwd; stat $${_RUNDIR}; cat /kubeconfig | head -n2; kubectl get nodes; \
	      if [ \"\$${SKIP_CLEANUP:-}\" != 'true' ]; then \
	          echo 'Running k8s cluster cleanup (invasive)...'; \
	          bash tests/bats/cleanup-from-previous-run.sh 2>&1 | tee -a $${_RUNDIR}/cleanup.outerr; \
	      else \
	          echo 'Skipping cleanup (SKIP_CLEANUP=true, ephemeral instance)'; \
	      fi; \
	      set +x; echo '--- STARTING TEST SUITE ---'; set -x; \
	      TMPDIR="$${_RUNDIR}" bats $(BATS_ARGS) $(1) \
      "
endef

default: tests

# Use flock to serialize local image builds in GHA CI. See
# https://github.com/kubernetes-sigs/dra-driver-nvidia-gpu/issues/908 and
# https://github.com/kubernetes-sigs/dra-driver-nvidia-gpu/issues/941.
.PHONY: runner-image
runner-image:
	@( \
		set -e; \
		if [ "$$GITHUB_ACTIONS" = "true" ]; then \
			echo "GHA CI: acquiring runner image build lock"; \
			exec 9>/tmp/gh-runner-locks/runner-image-build.lock; \
			_START=$$(date +%s.%N); \
			flock 9; \
			_END=$$(date +%s.%N); \
			_DUR=$$(awk "BEGIN {printf \"%.5f\", $$_END - $$_START}"); \
			echo "lock acquired after $$_DUR s"; \
		fi; \
		docker buildx build . -t $(BATS_IMAGE) -f tests/bats/Dockerfile; \
	)


# Warning: destructive against currently configured k8s cluster.
#
# Explicit invocation of 'cleanup-from-previous-run.sh' (could also be done as
# suite/file 'setup' in bats, but we'd lose output on success). During dev, you
# may want to add --show-output-of-passing-tests (and read bats docs for other
# cmdline args).
.PHONY: tests-gpu tests-gpu-single tests-gpu-a10 tests-gpu-dynmig tests-cd

# Single-GPU-safe subset. Suitable for CI environments with one GPU and no
# GPU Operator (e.g., Lambda Cloud). Excludes test_basics.bats (expects GPU
# Operator + clean state), MIG, stress, and upgrade tests.
# Mock NVML subset: GPU + CD tests on CPU-only nodes with mock NVML.
# MOCK_NVML=true causes tests requiring real CUDA/MPS/NVLink/IMEX to skip.
# Excludes DynMIG (MIG hardware), basics (expects GPU Operator), MIG, and stress.
tests-mock-nvml: MOCK_NVML := true
tests-mock-nvml: runner-image
	$(call RUN_BATS, \
		tests/bats/test_gpu_basic.bats \
		tests/bats/test_gpu_cuda_workloads.bats \
		tests/bats/test_gpu_sharing.bats \
		tests/bats/test_gpu_robustness.bats \
		tests/bats/test_gpu_extres.bats \
		tests/bats/test_gpu_stress.bats \
		tests/bats/test_gpu_updowngrade.bats \
		tests/bats/test_cd_imex_chan_inject.bats \
		tests/bats/test_cd_misc.bats \
		tests/bats/test_cd_logging.bats \
		tests/bats/test_cd_failover.bats \
		tests/bats/test_cd_mnnvl_workload.bats \
		tests/bats/test_cd_updowngrade.bats \
		tests/bats/test_cd_leader_election.bats)

tests-gpu-single: runner-image
	$(call RUN_BATS, \
		tests/bats/test_gpu_basic.bats \
		tests/bats/test_gpu_cuda_workloads.bats \
		tests/bats/test_gpu_local_ipc.bats \
		tests/bats/test_gpu_dynmig.bats \
		tests/bats/test_gpu_sharing.bats \
		tests/bats/test_gpu_robustness.bats \
		tests/bats/test_gpu_extres.bats)

# Stable single-GPU subset for pinned A10 Lambda CI. Derived from
# tests-gpu-single, but excludes DynMIG.
tests-gpu-a10: runner-image
	$(call RUN_BATS, \
		tests/bats/test_gpu_basic.bats \
		tests/bats/test_gpu_cuda_workloads.bats \
		tests/bats/test_gpu_local_ipc.bats \
		tests/bats/test_gpu_sharing.bats \
		tests/bats/test_gpu_robustness.bats \
		tests/bats/test_gpu_extres.bats)

# DynMIG-only subset for dedicated MIG-capable CI lanes.
tests-gpu-dynmig: runner-image
	$(call RUN_BATS, \
		tests/bats/test_gpu_dynmig.bats)

# Run a subset covering mainly the GPU plugin
tests-gpu: runner-image
	$(call RUN_BATS, \
		tests/bats/test_basics.bats \
		tests/bats/test_gpu_basic.bats \
		tests/bats/test_gpu_extres.bats \
		tests/bats/test_gpu_local_ipc.bats \
		tests/bats/test_gpu_dynmig.bats \
		tests/bats/test_gpu_mig.bats \
		tests/bats/test_gpu_updowngrade.bats \
		tests/bats/test_gpu_stress.bats)

# Run a subset covering mainly the CD plugin
tests-cd: runner-image
	$(call RUN_BATS, \
		tests/bats/test_basics.bats \
		tests/bats/test_cd_imex_chan_inject.bats \
		tests/bats/test_cd_mnnvl_workload.bats \
		tests/bats/test_cd_misc.bats \
		tests/bats/test_cd_logging.bats \
		tests/bats/test_cd_failover.bats \
		tests/bats/test_cd_updowngrade.bats \
		tests/bats/test_cd_leader_election.bats)

# Run the complete test suite (the reference!)
tests: runner-image
	$(call RUN_BATS, \
		tests/bats/test_basics.bats \
		tests/bats/test_gpu_basic.bats \
		tests/bats/test_gpu_extres.bats \
		tests/bats/test_gpu_local_ipc.bats \
		tests/bats/test_gpu_stress.bats \
		tests/bats/test_gpu_dynmig.bats \
		tests/bats/test_gpu_mig.bats \
		tests/bats/test_gpu_updowngrade.bats \
		tests/bats/test_cd_imex_chan_inject.bats \
		tests/bats/test_cd_mnnvl_workload.bats \
		tests/bats/test_cd_misc.bats \
		tests/bats/test_cd_logging.bats \
		tests/bats/test_cd_failover.bats \
		tests/bats/test_cd_updowngrade.bats \
		tests/bats/test_cd_leader_election.bats)
