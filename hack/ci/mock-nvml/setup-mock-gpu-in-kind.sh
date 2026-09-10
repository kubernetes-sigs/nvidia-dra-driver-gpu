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

# setup-mock-gpu-in-kind.sh -- Install mock NVML directly into a Kind node.
#
# Docker Desktop runs Linux containers inside a VM. Paths such as /dev,
# /proc, and /var/lib on macOS are therefore not the paths seen by the Kind
# node. This script prepares the mock driver in a disposable Linux
# container, then copies it directly into the Kind node with tar streams.
#
# Required environment:
#   KIND_NODE_CONTAINER  Docker container name of the Kind node
#
# Optional environment:
#   MOCK_GPU_BUILDER_IMAGE  Linux image with Go and a C toolchain
#                           (default: golang:1.26.4-bookworm)
set -o errexit
set -o nounset
set -o pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# shellcheck source=hack/ci/mock-nvml/common.sh
source "${SCRIPT_DIR}/common.sh"

: "${KIND_NODE_CONTAINER:?KIND_NODE_CONTAINER must name the Kind node container}"

MOCK_GPU_BUILDER_IMAGE="${MOCK_GPU_BUILDER_IMAGE:-golang:1.26.4-bookworm}"
MOCK_GPU_BUILDER_NAME="${KIND_NODE_CONTAINER}-mock-gpu-builder"
BUILDER_TEST_INFRA_ROOT="/work/k8s-test-infra"
BUILDER_SCRIPTS_ROOT="/work/dra-driver-mock-nvml"

cleanup_builder() {
  docker rm -f "${MOCK_GPU_BUILDER_NAME}" > /dev/null 2>&1 || true
}
trap cleanup_builder EXIT

if ! docker inspect "${KIND_NODE_CONTAINER}" > /dev/null 2>&1; then
  echo "ERROR: Kind node container does not exist: ${KIND_NODE_CONTAINER}" >&2
  exit 1
fi

if [ ! -d "${K8S_TEST_INFRA_DIR}/pkg/gpu/mocknvml" ]; then
  echo "ERROR: k8s-test-infra not found at ${K8S_TEST_INFRA_DIR}" >&2
  exit 1
fi

echo "=== Preparing mock GPU payload in Linux builder ==="
echo "Builder image: ${MOCK_GPU_BUILDER_IMAGE}"
echo "Kind node:     ${KIND_NODE_CONTAINER}"

cleanup_builder
docker run \
  --detach \
  --name "${MOCK_GPU_BUILDER_NAME}" \
  --entrypoint bash \
  "${MOCK_GPU_BUILDER_IMAGE}" \
  -c 'exec sleep infinity' > /dev/null

echo "Installing builder prerequisites..."
docker exec \
  -e DEBIAN_FRONTEND=noninteractive \
  "${MOCK_GPU_BUILDER_NAME}" \
  bash -c '
    apt-get update -qq
    apt-get install -y -qq ca-certificates curl gnupg patchelf sudo
    mkdir -p /work/k8s-test-infra /work/dra-driver-mock-nvml
  '

docker cp \
  "${K8S_TEST_INFRA_DIR}/." \
  "${MOCK_GPU_BUILDER_NAME}:${BUILDER_TEST_INFRA_ROOT}"
docker cp \
  "${SCRIPT_DIR}/common.sh" \
  "${MOCK_GPU_BUILDER_NAME}:${BUILDER_SCRIPTS_ROOT}/common.sh"
docker cp \
  "${SCRIPT_DIR}/setup-mock-gpu.sh" \
  "${MOCK_GPU_BUILDER_NAME}:${BUILDER_SCRIPTS_ROOT}/setup-mock-gpu.sh"

echo "Building mock NVML and rendering the mock driver filesystem..."
docker exec \
  -e GPU_PROFILE="${GPU_PROFILE}" \
  -e GPU_COUNT="${GPU_COUNT}" \
  -e DRIVER_VERSION="${DRIVER_VERSION}" \
  -e K8S_TEST_INFRA_DIR="${BUILDER_TEST_INFRA_ROOT}" \
  -e MOCK_ROOT="${MOCK_ROOT}" \
  -e DRIVER_ROOT="${DRIVER_ROOT}" \
  "${MOCK_GPU_BUILDER_NAME}" \
  bash -c '
    git config --global --add safe.directory /work/k8s-test-infra
    exec bash /work/dra-driver-mock-nvml/setup-mock-gpu.sh
  '

echo "Copying mock driver and CDI data into the Kind node..."
MOCK_ROOT_RELATIVE="${MOCK_ROOT#/}"
if [ -z "${MOCK_ROOT_RELATIVE}" ]; then
  echo "ERROR: MOCK_ROOT must not be /" >&2
  exit 1
fi

docker exec "${MOCK_GPU_BUILDER_NAME}" \
  tar -C / -cf - "${MOCK_ROOT_RELATIVE}" var/run/cdi/nvidia-mock.yaml |
  docker exec -i "${KIND_NODE_CONTAINER}" \
    tar -C / -xf -

echo "Configuring mock devices inside the Kind node..."
docker exec -i \
  -e DRIVER_ROOT="${DRIVER_ROOT}" \
  -e GPU_COUNT="${GPU_COUNT}" \
  -e MOCK_ROOT="${MOCK_ROOT}" \
  "${KIND_NODE_CONTAINER}" \
  bash -s <<'NODE_SETUP'
set -o errexit
set -o nounset
set -o pipefail

test -f /var/run/cdi/nvidia-mock.yaml
test -f "${DRIVER_ROOT}/usr/lib64/libnvidia-ml.so.1"
test -f "${MOCK_ROOT}/imex/proc-devices"

mkdir -p /run/nvidia /dev/nvidia-caps-imex-channels
ln -sfn "${DRIVER_ROOT}" /run/nvidia/driver

# Some tests discover host GPUs by scanning /dev.
for i in $(seq 0 $((GPU_COUNT - 1))); do
  ln -sfn "${MOCK_ROOT}/dev/nvidia${i}" "/dev/nvidia${i}"
done

# The compute-domain CDI spec references these paths directly.
for i in $(seq 0 2047); do
  channel="/dev/nvidia-caps-imex-channels/channel${i}"
  if [ ! -e "${channel}" ]; then
    mknod -m 666 "${channel}" c 235 "${i}"
  fi
done

"${DRIVER_ROOT}/usr/bin/nvidia-smi" -L
NODE_SETUP

echo "Mock GPU payload installed directly in ${KIND_NODE_CONTAINER}"
