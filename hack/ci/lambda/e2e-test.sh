#!/usr/bin/env bash
# Copyright The Kubernetes Authors.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# e2e-test.sh -- DRA GPU e2e tests on Lambda Cloud.
# Sources shared Lambda CI library from test-infra (available via Prow extra_refs).
#
# Required env: LAMBDA_API_KEY_FILE, JOB_NAME, BUILD_ID, ARTIFACTS (set by Prow)
# Optional env: GPU_TYPE (default: gpu_1x_a10), K8S_VERSION (default: latest stable), BATS_TARGET (default: tests-gpu-single)
set -o errexit
set -o nounset
set -o pipefail

# Locate shared library from test-infra checkout (Prow extra_refs).
TESTINFRA_DIR="$(go env GOPATH)/src/k8s.io/test-infra"
source "${TESTINFRA_DIR}/experiment/lambda/lib/lambda-common.sh"

# --- Launch Lambda instance ---
lambda_init_and_launch

# --- Download k8s binaries ---
K8S_VERSION="${K8S_VERSION:-}"
if [ -n "${K8S_VERSION}" ]; then
  lambda_download_k8s /tmp/k8s-bins "${K8S_VERSION}"
else
  lambda_download_k8s /tmp/k8s-bins
fi

# --- Detect k8s version for optional feature gates ---
# These Alpha feature gates must be explicitly enabled on k8s 1.35+:
#   DRAExtendedResource: allows nvidia.com/gpu in resources.limits (test_gpu_extres.bats)
#   DRAPartitionableDevices: allows SharedCounters/ConsumesCounters in ResourceSlices (DynamicMIG)
# DRAResourceClaimDeviceStatus lets the driver publish per-device status
# (KEP-4817) into ResourceClaim.status.devices (test_gpu_device_status.bats).
# Known since 1.32 and beta default-on since 1.33, so enabling it explicitly
# is safe on all supported versions.
if [ -n "${K8S_VERSION}" ]; then
  RESOLVED_K8S_VERSION="${K8S_VERSION}"
else
  RESOLVED_K8S_VERSION=$(curl -sL https://dl.k8s.io/release/stable.txt)
fi
K8S_MINOR=$(echo "${RESOLVED_K8S_VERSION}" | sed 's/v1\.\([0-9]*\)\..*/\1/')
KUBEADM_FEATURE_GATES=""
if [ "${K8S_MINOR}" -ge 35 ]; then
  KUBEADM_FEATURE_GATES="DRAExtendedResource=true,DRAPartitionableDevices=true,DRAResourceClaimDeviceStatus=true"
  echo "K8s >= 1.35 (${RESOLVED_K8S_VERSION}): enabling DRAExtendedResource,DRAPartitionableDevices,DRAResourceClaimDeviceStatus"
fi

# --- Compute git metadata before transfer ---
# The remote host needs GIT_COMMIT_SHORT for the BATS runner image tag.
# Compute it here where we have a real git repo.
GIT_COMMIT_SHORT="$(git rev-parse --short=8 HEAD)"
export GIT_COMMIT_SHORT

# --- Transfer artifacts ---
# Exclude .git (GIT_COMMIT_SHORT is passed explicitly), .claude, dist, _output.
# vendor/ is included (Dockerfile uses -mod=vendor).
lambda_rsync_to /tmp/k8s-bins/ "ubuntu@${LAMBDA_INSTANCE_IP}:/tmp/k8s-bins/"
lambda_rsync_to --exclude=.git --exclude=.claude --exclude=dist --exclude=_output \
  ./ "ubuntu@${LAMBDA_INSTANCE_IP}:/tmp/dra-driver-nvidia-gpu/"

# --- Setup cluster (generic, from test-infra) ---
# Pass DRA-specific options as env vars: CDI for device injection, Docker for
# BATS harness, node label for the Helm chart's DaemonSet affinity.
# `env VAR=val` is used because `bash -s VAR=val` makes them positional args, not env.
lambda_remote env \
  ENABLE_CDI=true \
  ENABLE_DOCKER=true \
  NODE_LABELS=nvidia.com/gpu.present=true \
  KUBEADM_FEATURE_GATES="${KUBEADM_FEATURE_GATES}" \
  bash -s < "${TESTINFRA_DIR}/experiment/lambda/lib/setup-k8s-node.sh"

# --- Build driver image on Lambda and load into containerd ---
lambda_remote bash -s <<EOF
set -euxo pipefail
cd /tmp/dra-driver-nvidia-gpu
docker buildx use default 2>/dev/null || true
make -f deployments/container/Makefile build DOCKER_BUILD_OPTIONS="--load" CI=true
IMAGE_REF=\$(make -f deployments/container/Makefile -s print-IMAGE)
docker save "\${IMAGE_REF}" | sudo ctr -n k8s.io images import -
echo "Image loaded: \${IMAGE_REF}"
EOF

# --- Detect GPU capabilities for dynamic test filtering ---
GPU_COUNT=$(lambda_remote nvidia-smi -L | wc -l)
echo "Detected ${GPU_COUNT} GPU(s) on Lambda instance (type: ${LAMBDA_GPU_TYPE})"

# Build filter tags based on what the instance can handle.
FILTER='!version-specific'
if [ "${GPU_COUNT}" -lt 2 ]; then
  FILTER="${FILTER},!multi-gpu"
fi
# busGrind ships only in NVIDIA's cuda-demo-suite-* apt package, which is
# published for x86_64 but not for sbsa/arm64 (confirmed against NVIDIA's
# public CUDA apt repo). On V100/A10 it is also prone to hang. Gate
# accordingly; revisit the gh200 entry if NVIDIA ever publishes
# cuda-demo-suite for sbsa.
case "${LAMBDA_GPU_TYPE}" in
  *v100*|*a10|*gh200*) FILTER="${FILTER},!gpu-busgrind" ;;
esac
# Dynamic MIG requires MIG-capable GPUs (A100/H100/GH200/B200).
# On single-GPU A100 instances, DynMIG fails with "In use by another client"
# because the driver holds the only GPU via NVML, blocking MIG mode toggle.
# Skip DynMIG on single-GPU A100; it works on multi-GPU A100 (8x) and H100+.
case "${LAMBDA_GPU_TYPE}" in
  *a100*)
    if [ "${GPU_COUNT}" -lt 2 ]; then
      FILTER="${FILTER},!dynmig"
    fi
    ;;
  *h100*|*gh200*|*b200*) ;;
  *) FILTER="${FILTER},!dynmig" ;;
esac

# Detect whether compute domains should be disabled.
DISABLE_CD=true
case "${LAMBDA_GPU_TYPE}" in
  *gb200*|*gb300*|*b200*) DISABLE_CD=false ;;
esac
echo "Test filter: ${FILTER}, compute domains disabled: ${DISABLE_CD}"

# Default CI target, overridable by test-infra job config.
BATS_TARGET="${BATS_TARGET:-tests-gpu-single}"

# --- Pre-cleanup: MIG teardown on host ---
# Run MIG cleanup directly on the host where nvidia-smi is available.
# The BATS Docker container uses the lambda nvmm stub (no-op).
# We handle MIG cleanup here instead.
# IMPORTANT: Skip on A100 — disabling MIG mode on cloud VM A100s can put the
# GPU in an unrecoverable state (#883).
case "${LAMBDA_GPU_TYPE}" in
  *a100*) echo "Skipping MIG cleanup on A100" ;;
  *) lambda_remote sh -c 'nvidia-smi mig -dci 2>/dev/null; nvidia-smi mig -dgi 2>/dev/null; nvidia-smi -mig 0 2>/dev/null; echo "MIG cleanup done"' || true ;;
esac

# --- Run BATS tests ---
# Tests local artifacts: local chart + local image built from the PR.
# This is a real presubmit -- it validates the repo's code, chart, and specs.
lambda_remote bash -s <<EOF
set -euxo pipefail
cd /tmp/dra-driver-nvidia-gpu

# Build the BATS runner image.
# GIT_COMMIT_SHORT was computed on the Prow pod; pass it explicitly so
# versions.mk doesn't need a functional .git directory on the remote host.
docker buildx use default 2>/dev/null || true
make -f tests/bats/Makefile runner-image GIT_COMMIT_SHORT=${GIT_COMMIT_SHORT}

export KUBECONFIG=\$HOME/.kube/config
export CI=true
export TEST_NVIDIA_DRIVER_ROOT=/
export TEST_CHART_LOCAL=true
export DISABLE_COMPUTE_DOMAINS=${DISABLE_CD}
export TEST_FILTER_TAGS='${FILTER}'
export GIT_COMMIT_SHORT=${GIT_COMMIT_SHORT}
echo "Running BATS target: ${BATS_TARGET}"
# Use lambda nvmm stub (no GPU Operator). MIG cleanup handled above.
export NVMM_PATH=/cwd/tests/bats/lib/lambda

make -f tests/bats/Makefile ${BATS_TARGET} GIT_COMMIT_SHORT=${GIT_COMMIT_SHORT}
EOF

# --- Collect artifacts ---
# CI=true in the Makefile sets TMPDIR=$(CURDIR), so artifacts land under the repo dir.
lambda_collect_artifacts /tmp/dra-driver-nvidia-gpu/k8s-dra-driver-gpu-tests-out-ubuntu/

# Cluster debug info
lambda_remote bash -s <<'DEBUGEOF' > "${ARTIFACTS}/cluster-debug.txt" 2>&1 || true
export KUBECONFIG=$HOME/.kube/config
echo "=== nvidia-smi ===" && nvidia-smi
echo "=== nvidia driver version ===" && cat /sys/module/nvidia/version
echo "=== nodes ===" && kubectl get nodes -o wide
echo "=== pods (all namespaces) ===" && kubectl get pods -A -o wide
echo "=== pod describe (driver namespace) ===" && kubectl describe pods -n dra-driver-nvidia-gpu
echo "=== resourceslices ===" && kubectl get resourceslices -A -o yaml
echo "=== deviceclasses ===" && kubectl get deviceclass -o yaml
echo "=== resourceclaims ===" && kubectl get resourceclaims -A -o yaml
echo "=== events ===" && kubectl get events -A --sort-by=.lastTimestamp
echo "=== kubelet-plugin logs (gpus) ===" && kubectl logs -n dra-driver-nvidia-gpu -l dra-driver-nvidia-gpu-component=kubelet-plugin -c gpus --tail=500
echo "=== kubelet-plugin logs (compute-domains) ===" && kubectl logs -n dra-driver-nvidia-gpu -l dra-driver-nvidia-gpu-component=kubelet-plugin -c compute-domains --tail=200
echo "=== controller logs ===" && kubectl logs -n dra-driver-nvidia-gpu -l dra-driver-nvidia-gpu-component=controller --tail=200
echo "=== containerd config ===" && sudo grep -E "enable_cdi|default_runtime|nvidia" /etc/containerd/config.toml
echo "=== kubelet logs (last 100 lines) ===" && sudo journalctl -u kubelet --no-pager -n 100
DEBUGEOF
