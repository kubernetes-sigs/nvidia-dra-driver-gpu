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

# e2e-test.sh -- DRA GPU e2e tests on CPU-only nodes using mock NVML.
#
# This is the mock-NVML equivalent of hack/ci/lambda/e2e-test.sh. Instead of
# running on Lambda Cloud instances with real GPUs, it:
#   1. Sets up mock NVML on the local host (or Kind node)
#   2. Creates a Kind cluster with DRA support
#   3. Installs the nvml-mock Helm chart to expose fake GPUs
#   4. Builds and loads the DRA driver image
#   5. Runs the BATS test suite with appropriate filters
#
# This enables GPU-related CI on cheap CPU-only machines (Prow, GitHub Actions,
# Brev instances).
#
# Usage:
#   GPU_PROFILE=gb200 GPU_COUNT=8 ./hack/ci/mock-nvml/e2e-test.sh
#
# Required:
#   - Docker
#   - Kind (or will be installed)
#   - Helm (or will be installed)
#   - kubectl (or will be installed)
#   - Go 1.25+ (for building mock NVML)
#   - k8s-test-infra checkout (for mock NVML source)
#
# Environment:
#   GPU_PROFILE         GPU profile (default: gb200)
#   GPU_COUNT           GPUs to emulate (default: 8)
#   K8S_VERSION         Kubernetes version for Kind (default: latest)
#   K8S_TEST_INFRA_DIR  Path to k8s-test-infra (default: GOPATH-based)
#   KIND_CLUSTER_NAME   Kind cluster name (default: mock-nvml-test)
#   ARTIFACTS           Artifact output directory (default: /tmp/mock-nvml-artifacts)
#   SKIP_CLEANUP        Don't delete cluster on exit (default: false)
set -o errexit
set -o nounset
set -o pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../../.." && pwd)"

# shellcheck source=hack/ci/mock-nvml/common.sh
source "${SCRIPT_DIR}/common.sh"
K8S_VERSION="${K8S_VERSION:-}"
KIND_CLUSTER_NAME="${KIND_CLUSTER_NAME:-mock-nvml-test}"
ARTIFACTS="${ARTIFACTS:-/tmp/mock-nvml-artifacts}"
SKIP_CLEANUP="${SKIP_CLEANUP:-false}"

GIT_COMMIT_SHORT="$(git -C "${REPO_ROOT}" rev-parse --short=8 HEAD)"
export GIT_COMMIT_SHORT

mkdir -p "${ARTIFACTS}"

echo "=== Mock NVML E2E Test ==="
echo "Profile:     ${GPU_PROFILE}"
echo "GPU count:   ${GPU_COUNT}"
echo "K8s version: ${K8S_VERSION:-latest}"
echo "Cluster:     ${KIND_CLUSTER_NAME}"
echo "Artifacts:   ${ARTIFACTS}"
echo "Commit:      ${GIT_COMMIT_SHORT}"

# --- Cleanup on exit ---
cleanup() {
  local exit_code=$?
  echo ""
  echo "=== Collecting artifacts ==="
  collect_artifacts || true

  if [ "${SKIP_CLEANUP}" != "true" ]; then
    echo "=== Deleting Kind cluster ==="
    kind delete cluster --name "${KIND_CLUSTER_NAME}" 2>/dev/null || true
    echo "=== Cleaning up mock GPU host artifacts ==="
    bash "${SCRIPT_DIR}/cleanup-mock-gpu.sh" || true
  else
    echo "Skipping cleanup (SKIP_CLEANUP=true)"
  fi

  exit "${exit_code}"
}
trap cleanup EXIT

collect_artifacts() {
  local debug_file="${ARTIFACTS}/cluster-debug.txt"
  {
    echo "=== mock nvidia-smi (host) ==="
    "${DRIVER_ROOT}/usr/bin/nvidia-smi" 2>&1 || echo "(nvidia-smi not available)"

    if kubectl cluster-info > /dev/null 2>&1; then
      echo "=== nodes ==="
      kubectl get nodes -o wide
      echo "=== pods (all namespaces) ==="
      kubectl get pods -A -o wide
      echo "=== pod describe (driver namespace) ==="
      kubectl describe pods -n dra-driver-nvidia-gpu 2>/dev/null || true
      echo "=== resourceslices ==="
      kubectl get resourceslices -A -o yaml 2>/dev/null || true
      echo "=== deviceclasses ==="
      kubectl get deviceclass -o yaml 2>/dev/null || true
      echo "=== resourceclaims ==="
      kubectl get resourceclaims -A -o yaml 2>/dev/null || true
      echo "=== events ==="
      kubectl get events -A --sort-by=.lastTimestamp 2>/dev/null || true
      echo "=== kubelet-plugin logs (gpus) ==="
      kubectl logs -n dra-driver-nvidia-gpu -l dra-driver-nvidia-gpu-component=kubelet-plugin -c gpus --tail=500 2>/dev/null || true
      echo "=== kubelet-plugin logs (compute-domains) ==="
      kubectl logs -n dra-driver-nvidia-gpu -l dra-driver-nvidia-gpu-component=kubelet-plugin -c compute-domains --tail=200 2>/dev/null || true
      echo "=== controller logs ==="
      kubectl logs -n dra-driver-nvidia-gpu -l dra-driver-nvidia-gpu-component=controller --tail=200 2>/dev/null || true
    fi
  } > "${debug_file}" 2>&1

  # Export Kind cluster logs (containerd, kubelet, etcd, etc.)
  kind export logs "${ARTIFACTS}/kind-logs" --name "${KIND_CLUSTER_NAME}" 2>/dev/null || true
  echo "Artifacts written to ${ARTIFACTS}/"
}

# --- Step 1: Install prerequisites ---
echo ""
echo "--- Step 1: Checking prerequisites ---"
for tool in docker go git; do
  if ! command -v "${tool}" > /dev/null 2>&1; then
    echo "ERROR: ${tool} is required but not found" >&2
    exit 1
  fi
done

# Install kind if missing
if ! command -v kind > /dev/null 2>&1; then
  echo "Installing kind..."
  go install sigs.k8s.io/kind@latest
fi

# Install helm if missing
if ! command -v helm > /dev/null 2>&1; then
  echo "Installing helm..."
  curl -fsSL https://raw.githubusercontent.com/helm/helm/main/scripts/get-helm-3 | bash
fi

# Install kubectl if missing
if ! command -v kubectl > /dev/null 2>&1; then
  echo "Installing kubectl..."
  KUBECTL_ARCH="$(uname -m)"
  case "${KUBECTL_ARCH}" in
    x86_64|amd64) KUBECTL_ARCH="amd64" ;;
    aarch64|arm64) KUBECTL_ARCH="arm64" ;;
    ppc64le|s390x) ;;
    *)
      echo "ERROR: unsupported architecture for kubectl install: ${KUBECTL_ARCH}" >&2
      exit 1
      ;;
  esac
  KUBECTL_VERSION="$(curl -sL https://dl.k8s.io/release/stable.txt)"
  curl -fsSLo /tmp/kubectl "https://dl.k8s.io/release/${KUBECTL_VERSION}/bin/linux/${KUBECTL_ARCH}/kubectl"
  sudo install /tmp/kubectl /usr/local/bin/kubectl
fi

echo "All prerequisites available"

# --- Step 2: Set up mock NVML on host ---
echo ""
echo "--- Step 2: Setting up mock NVML on host ---"
GPU_PROFILE="${GPU_PROFILE}" \
GPU_COUNT="${GPU_COUNT}" \
K8S_TEST_INFRA_DIR="${K8S_TEST_INFRA_DIR}" \
DRIVER_VERSION="${DRIVER_VERSION}" \
  bash "${SCRIPT_DIR}/setup-mock-gpu.sh"

# Quick sanity check
echo ""
echo "--- Verifying mock GPU setup ---"
${DRIVER_ROOT}/usr/bin/nvidia-smi -L
echo "Host nvidia-smi OK"

# --- Step 3: Detect k8s version for feature gates ---
echo ""
echo "--- Step 3: Detecting Kubernetes version ---"
if [ -n "${K8S_VERSION}" ]; then
  RESOLVED_K8S_VERSION="${K8S_VERSION}"
else
  RESOLVED_K8S_VERSION=$(curl -sL https://dl.k8s.io/release/stable.txt)
fi
K8S_MINOR=$(echo "${RESOLVED_K8S_VERSION}" | sed 's/v1\.\([0-9]*\)\..*/\1/')

FEATURE_GATES=""
if [ "${K8S_MINOR}" -ge 35 ]; then
  FEATURE_GATES="DRAExtendedResource=true,DRAPartitionableDevices=true"
  echo "K8s >= 1.35 (${RESOLVED_K8S_VERSION}): enabling DRAExtendedResource,DRAPartitionableDevices"
fi
TEST_DRA_LIST_TYPE_ATTRIBUTES=false
if [ "${K8S_MINOR}" -ge 37 ]; then
  TEST_DRA_LIST_TYPE_ATTRIBUTES=true
  echo "K8s >= 1.37 (${RESOLVED_K8S_VERSION}): enabling DRAListTypeAttributes"
fi
export TEST_DRA_LIST_TYPE_ATTRIBUTES

# --- Step 4: Create Kind cluster ---
echo ""
echo "--- Step 4: Creating Kind cluster ---"

# Delete existing cluster if present
kind delete cluster --name "${KIND_CLUSTER_NAME}" 2>/dev/null || true

# Build Kind config with mock NVML host paths mounted into nodes.
# The Kind node needs access to:
#   /var/lib/nvml-mock  -- mock driver root, device nodes, config
#   /var/run/cdi        -- CDI spec for container runtime
#   /run/nvidia         -- GPU Operator compat symlink
KIND_CONFIG=$(mktemp)
cat > "${KIND_CONFIG}" << 'KINDEOF'
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
featureGates:
  DynamicResourceAllocation: true
nodes:
  - role: control-plane
    extraMounts:
      # Mount mock driver root so kubelet-plugin can discover GPUs
      - hostPath: /var/lib/nvml-mock
        containerPath: /var/lib/nvml-mock
        readOnly: false
      # Mount CDI spec so containerd can inject mock devices
      - hostPath: /var/run/cdi
        containerPath: /var/run/cdi
        readOnly: false
      # GPU Operator compat symlink target
      - hostPath: /run/nvidia
        containerPath: /run/nvidia
        readOnly: false
    kubeadmConfigPatches:
      - |
        kind: InitConfiguration
        nodeRegistration:
          kubeletExtraArgs:
            v: "3"
    labels:
      nvidia.com/gpu.present: "true"
      feature.node.kubernetes.io/pci-10de.present: "true"
containerdConfigPatches:
  - |-
    [plugins."io.containerd.grpc.v1.cri"]
      enable_cdi = true
KINDEOF

# Inject additional feature gates for k8s 1.35+
if [ -n "${FEATURE_GATES}" ]; then
  # Add DRA-specific feature gates to the Kind config
  sed -i '/DynamicResourceAllocation: true/a\  DRAExtendedResource: true\n  DRAPartitionableDevices: true' "${KIND_CONFIG}"
fi
if [ "${TEST_DRA_LIST_TYPE_ATTRIBUTES}" = "true" ]; then
  sed -i '/DynamicResourceAllocation: true/a\  DRAListTypeAttributes: true' "${KIND_CONFIG}"
fi
# KEP-4680 device health in pod status (tests/bats/test_gpu_health.bats):
# beta and on by default from k8s 1.36, alpha (off) before.
if [ "${K8S_MINOR}" -lt 36 ]; then
  echo "K8s < 1.36 (${RESOLVED_K8S_VERSION}): enabling ResourceHealthStatus"
  sed -i '/DynamicResourceAllocation: true/a\  ResourceHealthStatus: true' "${KIND_CONFIG}"
fi

# Select Kind node image if k8s version specified
KIND_IMAGE_ARG=""
if [ -n "${K8S_VERSION}" ]; then
  KIND_IMAGE_ARG="--image kindest/node:${K8S_VERSION}"
fi

kind create cluster \
  --name "${KIND_CLUSTER_NAME}" \
  --config "${KIND_CONFIG}" \
  ${KIND_IMAGE_ARG}

rm -f "${KIND_CONFIG}"

# Wait for node to be ready
kubectl wait --for=condition=Ready nodes --all --timeout=120s
echo "Kind cluster ready"

# Verify CDI is visible inside Kind node
echo "Verifying CDI spec visible in Kind node..."
docker exec "${KIND_CLUSTER_NAME}-control-plane" ls /var/run/cdi/nvidia-mock.yaml
docker exec "${KIND_CLUSTER_NAME}-control-plane" ls /var/lib/nvml-mock/driver/usr/lib64/libnvidia-ml.so.1

# Create /dev/nvidia* symlinks in the Kind node so tests that scan the host's
# /dev directory (test_gpu_robustness "ResourceSlice device count matches host
# GPU count") can find the mock device nodes.
for i in $(seq 0 $((GPU_COUNT - 1))); do
  docker exec "${KIND_CLUSTER_NAME}-control-plane" \
    ln -sf "/var/lib/nvml-mock/dev/nvidia${i}" "/dev/nvidia${i}"
done

# Create mock IMEX channel device nodes directly inside the Kind node.
# Kind extraMounts into /dev/ don't work (Docker manages /dev via devtmpfs),
# so we mknod them directly. The compute-domain CDI spec references
# /dev/nvidia-caps-imex-channels/channelN as both container and host path.
IMEX_MAJOR=235
docker exec "${KIND_CLUSTER_NAME}-control-plane" mkdir -p /dev/nvidia-caps-imex-channels
echo "Creating 2048 IMEX channel device nodes inside Kind node..."
docker exec "${KIND_CLUSTER_NAME}-control-plane" sh -c "
  for i in \$(seq 0 2047); do
    mknod -m 666 /dev/nvidia-caps-imex-channels/channel\${i} c ${IMEX_MAJOR} \${i} 2>/dev/null || true
  done
"
echo "IMEX channel device nodes created in Kind node"

# Bind-mount fake /proc/devices into the Kind node so the compute-domain
# kubelet-plugin can find nvidia-caps-imex-channels. This requires nsenter
# because plain `docker exec mount --bind` cannot bind-mount over /proc
# entries from within the container.
MOCK_PROC_DEVICES="${MOCK_ROOT}/imex/proc-devices"
if [ -f "${MOCK_PROC_DEVICES}" ]; then
  KIND_PID=$(docker inspect -f '{{.State.Pid}}' "${KIND_CLUSTER_NAME}-control-plane")
  sudo cp "${MOCK_PROC_DEVICES}" "/proc/${KIND_PID}/root/tmp/mock-proc-devices"
  if sudo nsenter -t "${KIND_PID}" -m -p -- mount --bind /tmp/mock-proc-devices /proc/devices 2>/dev/null; then
    echo "Mock /proc/devices installed in Kind node"
  else
    echo "WARNING: Could not bind-mount mock /proc/devices (compute-domains will be disabled)"
  fi
fi

# --- Step 5: Build DRA driver image and load into Kind ---
echo ""
echo "--- Step 5: Building DRA driver image ---"
cd "${REPO_ROOT}"
docker buildx use default 2>/dev/null || true
make -f deployments/container/Makefile build DOCKER_BUILD_OPTIONS="--load" CI=true

IMAGE_REF=$(make -f deployments/container/Makefile -s print-IMAGE)
echo "Built image: ${IMAGE_REF}"

kind load docker-image "${IMAGE_REF}" --name "${KIND_CLUSTER_NAME}"
echo "Image loaded into Kind"

# --- Step 6: Build test filter tags ---
echo ""
echo "--- Step 6: Computing test filter tags ---"
FILTER='!version-specific'

# Mock GPUs: no real CUDA compute, so skip CUDA workload tests
FILTER="${FILTER},!cuda-workload"

# Mock GPUs: MIG mode toggle isn't supported (returns NOT_SUPPORTED)
FILTER="${FILTER},!dynmig,!mig"

# Mock GPUs: no real busGrind
FILTER="${FILTER},!gpu-busgrind"

# Compute domains require multi-node NVLink hardware
FILTER="${FILTER},!compute-domain"

# Single-node Kind: skip multi-node tests
FILTER="${FILTER},!multi-node"

if [ "${GPU_COUNT}" -lt 2 ]; then
  FILTER="${FILTER},!multi-gpu"
fi

echo "Test filter: ${FILTER}"

# --- Step 7: Run BATS tests ---
echo ""
echo "--- Step 7: Running BATS tests ---"
cd "${REPO_ROOT}"

# Build the BATS runner image.
# GHA CI uses flock to serialize image builds (see tests/bats/Makefile).
mkdir -p /tmp/gh-runner-locks 2>/dev/null || true
docker buildx use default 2>/dev/null || true
make -f tests/bats/Makefile runner-image GIT_COMMIT_SHORT="${GIT_COMMIT_SHORT}"

export KUBECONFIG="${HOME}/.kube/config"
export CI=true
export TEST_NVIDIA_DRIVER_ROOT="${DRIVER_ROOT}"
export TEST_CHART_LOCAL=true
export DISABLE_COMPUTE_DOMAINS=false
export TEST_FILTER_TAGS="${FILTER}"
export NVMM_PATH=/cwd/tests/bats/lib/lambda
export TEST_ALT_PROC_DEVICES="${MOCK_ROOT}/imex/proc-devices"
# Kind kubeconfig uses 127.0.0.1; the BATS Docker container needs host networking.
export DOCKER_NETWORK=host

# Run mock-compatible test files only. We skip test_gpu_cuda_workloads.bats
# because it includes a CUDA demo suite test that requires real GPU compute
# and has a 15-minute timeout. We also skip test_gpu_dynmig.bats (MIG).
# TMPDIR=/tmp overrides the Makefile's CI default (CURDIR) so the BATS run
# directory lands under /tmp which is mounted into the Docker container.
SKIP_CLEANUP=true \
  make -f tests/bats/Makefile tests-mock-nvml \
    GIT_COMMIT_SHORT="${GIT_COMMIT_SHORT}" \
    TMPDIR=/tmp

echo ""
echo "=== Mock NVML E2E Summary ==="
echo "Profile: ${GPU_PROFILE}, GPUs: ${GPU_COUNT}, Driver: ${DRIVER_VERSION}"
echo "k8s-test-infra: ${K8S_TEST_INFRA_DIR}"
echo "BATS target: tests-mock-nvml (MOCK_NVML=true)"
echo "All tests passed or skipped. See ok/skip lines above for details."
