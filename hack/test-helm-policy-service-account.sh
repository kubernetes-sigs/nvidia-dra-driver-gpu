#!/bin/bash
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

set -euo pipefail

REPO_ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
CHART_PATH="${REPO_ROOT}/deployments/helm/dra-driver-nvidia-gpu"
RELEASE_NAME=validating-admission-policy-test
NAMESPACE=validating-admission-policy-test

kubelet_plugin=$(helm template "${RELEASE_NAME}" "${CHART_PATH}" \
    --namespace "${NAMESPACE}" \
    --set gpuResourcesEnabledOverride=true \
    --show-only templates/kubeletplugin.yaml)
policy=$(helm template "${RELEASE_NAME}" "${CHART_PATH}" \
    --namespace "${NAMESPACE}" \
    --set gpuResourcesEnabledOverride=true \
    --show-only templates/validatingadmissionpolicy.yaml)

mapfile -t service_accounts < <(
    sed -nE 's/^[[:space:]]*serviceAccountName:[[:space:]]*([^[:space:]#]+).*$/\1/p' \
        <<<"${kubelet_plugin}"
)
if [[ ${#service_accounts[@]} -ne 1 ]]; then
    echo "expected one kubelet-plugin serviceAccountName, found ${#service_accounts[@]}" >&2
    exit 1
fi

expected="request.userInfo.username == \"system:serviceaccount:${NAMESPACE}:${service_accounts[0]}\""
if [[ "${policy}" != *"${expected}"* ]]; then
    echo "ValidatingAdmissionPolicy does not target the kubelet-plugin service account" >&2
    echo "expected: ${expected}" >&2
    exit 1
fi
