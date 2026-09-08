# shellcheck disable=SC2148
# shellcheck disable=SC2329

# Tests for KEP-4817 ResourceClaim device status publishing by the GPU
# kubelet plugin: after a successful prepare the plugin writes one entry per
# prepared device into ResourceClaim.status.devices, and removes its entries
# on unprepare. Requires the ResourceClaimDeviceStatus driver feature gate
# (enabled via setup_file below) and the cluster-side
# DRAResourceClaimDeviceStatus gate (beta default-on since Kubernetes 1.33,
# GA locked since 1.37). Works on any GPU type, including mock NVML: no CUDA
# compute is required.

setup_file() {
  load 'helpers.sh'
  _common_setup
  local _iargs=("--set" "logVerbosity=6"
    "--set" "featureGates.ResourceClaimDeviceStatus=true")
  if [ "${DISABLE_COMPUTE_DOMAINS:-}" = "true" ]; then
    _iargs+=("--set" "resources.computeDomains.enabled=false")
  fi
  iupgrade_wait "${TEST_CHART_REPO}" "${TEST_CHART_VERSION}" _iargs
}

setup() {
  load 'helpers.sh'
  _common_setup
  log_objects
}

bats::on_failure() {
  echo -e "\n\nFAILURE HOOK START"
  log_objects
  show_kubelet_plugin_error_logs
  show_gpu_plugin_log_tails
  echo -e "FAILURE HOOK END\n\n"
}

# Skip unless the deployed kubelet plugin runs with the
# ResourceClaimDeviceStatus driver feature gate enabled (rendered into the
# FEATURE_GATES env var by the Helm chart).
require_device_status_driver_gate() {
  local gates
  gates=$(kubectl get daemonset -n dra-driver-nvidia-gpu \
    dra-driver-nvidia-gpu-kubelet-plugin -o json 2>/dev/null | \
    jq -r '.spec.template.spec.containers[].env[]? | select(.name=="FEATURE_GATES") | .value')
  if ! echo "${gates}" | grep -q "ResourceClaimDeviceStatus=true"; then
    skip "ResourceClaimDeviceStatus driver feature gate is not enabled on the deployed driver"
  fi
}

# Skip unless the API server serves the status.devices field (i.e. the
# cluster-side DRAResourceClaimDeviceStatus gate is enabled).
require_device_status_cluster_gate() {
  if ! kubectl explain resourceclaim.status.devices >/dev/null 2>&1; then
    skip "cluster does not expose resourceclaim.status.devices (DRAResourceClaimDeviceStatus not enabled)"
  fi
}

# Skip when the plugin's status write was dropped by the API server, which
# happens when the cluster-side DRAResourceClaimDeviceStatus gate is disabled
# despite the field being served. The plugin logs a warning in that case (see
# cmd/gpu-kubelet-plugin/devicestatus.go).
skip_if_status_write_dropped() {
  local pod
  for pod in $(kubectl get pods -n dra-driver-nvidia-gpu \
      -l dra-driver-nvidia-gpu-component=kubelet-plugin \
      -o jsonpath='{.items[*].metadata.name}'); do
    if kubectl logs -n dra-driver-nvidia-gpu "${pod}" -c gpus \
        --tail=500 2>/dev/null | grep -q "were dropped by the API server"; then
      skip "DRAResourceClaimDeviceStatus is not enabled on the API server (status write dropped)"
    fi
  done
}

# Poll up to $1 seconds for at least one gpu.nvidia.com entry in
# .status.devices of claim $2. Emits the claim JSON to stdout on success.
wait_for_device_status() {
  local timeout="$1"
  local claim="$2"
  local start=$SECONDS
  while (( SECONDS - start < timeout )); do
    local count
    count=$(kubectl get resourceclaim "${claim}" -o json 2>/dev/null | \
      jq '[.status.devices[]? | select(.driver=="gpu.nvidia.com")] | length')
    if [ -n "${count}" ] && [ "${count}" -ge 1 ]; then
      kubectl get resourceclaim "${claim}" -o json
      return 0
    fi
    sleep 2
  done
  return 1
}

# Poll up to $1 seconds until .status.devices of claim $2 is empty.
wait_for_device_status_pruned() {
  local timeout="$1"
  local claim="$2"
  local start=$SECONDS
  while (( SECONDS - start < timeout )); do
    local count
    count=$(kubectl get resourceclaim "${claim}" -o json 2>/dev/null | \
      jq '.status.devices // [] | length')
    if [ -n "${count}" ] && [ "${count}" -eq 0 ]; then
      return 0
    fi
    sleep 2
  done
  return 1
}


# bats test_tags=fastfeedback,device-status
@test "GPUs: publish, keep and prune ResourceClaim.status.devices across the claim lifecycle" {
  require_device_status_driver_gate
  require_device_status_cluster_gate

  local _claim="rc-device-status"
  local _pod1="pod-device-status-1"
  local _pod2="pod-device-status-2"

  # Prepare: one pod on a standalone claim.
  kubectl apply -f tests/bats/specs/gpu-device-status.yaml
  kubectl wait --for=condition=READY pods "${_pod1}" --timeout=60s

  local claim_json
  if ! claim_json=$(wait_for_device_status 120 "${_claim}"); then
    skip_if_status_write_dropped
    echo "timed out waiting for status.devices on claim ${_claim}"
    kubectl get resourceclaim "${_claim}" -o yaml
    return 1
  fi

  # The entry must describe the allocated device ...
  local pool device
  pool=$(echo "${claim_json}" | jq -r '.status.allocation.devices.results[0].pool')
  device=$(echo "${claim_json}" | jq -r '.status.allocation.devices.results[0].device')
  [ -n "${pool}" ] && [ "${pool}" != "null" ] || fail "claim has no allocated pool"
  [ -n "${device}" ] && [ "${device}" != "null" ] || fail "claim has no allocated device"

  run jq -e --arg pool "${pool}" --arg device "${device}" \
    '.status.devices[] | select(.driver=="gpu.nvidia.com" and .pool==$pool and .device==$device and .data != null)' <<<"${claim_json}"
  assert_success

  # ... with a full-GPU payload ...
  local dtype uuid product_name driver_version pci_bus_id
  dtype=$(echo "${claim_json}" | jq -r '[.status.devices[] | select(.driver=="gpu.nvidia.com")][0].data.type')
  assert_equal "${dtype}" "gpu"

  uuid=$(echo "${claim_json}" | jq -r '[.status.devices[] | select(.driver=="gpu.nvidia.com")][0].data.uuid')
  [[ "${uuid}" == GPU-* ]] || fail "expected data.uuid with GPU- prefix, got: ${uuid}"

  product_name=$(echo "${claim_json}" | jq -r '[.status.devices[] | select(.driver=="gpu.nvidia.com")][0].data.productName')
  [ -n "${product_name}" ] && [ "${product_name}" != "null" ] || fail "data.productName is empty"

  driver_version=$(echo "${claim_json}" | jq -r '[.status.devices[] | select(.driver=="gpu.nvidia.com")][0].data.driverVersion')
  [ -n "${driver_version}" ] && [ "${driver_version}" != "null" ] || fail "data.driverVersion is empty"

  pci_bus_id=$(echo "${claim_json}" | jq -r '[.status.devices[] | select(.driver=="gpu.nvidia.com")][0].data.pciBusID')
  [ -n "${pci_bus_id}" ] && [ "${pci_bus_id}" != "null" ] || fail "data.pciBusID is empty"

  # ... and the UUID must match the ResourceSlice attribute of the allocated
  # device.
  local slice_uuid
  slice_uuid=$(kubectl get resourceslices.resource.k8s.io -o json | jq -r \
    --arg pool "${pool}" --arg device "${device}" \
    '[.items[] | select(.spec.driver=="gpu.nvidia.com" and .spec.pool.name==$pool) | .spec.devices[]? | select(.name==$device) | .attributes.uuid.string] | .[0]')
  assert_equal "${uuid}" "${slice_uuid}"

  # A second consumer of the same claim must not disturb the entry
  # (idempotent prepare).
  local before
  before=$(echo "${claim_json}" | jq -c -S '.status.devices')

  kubectl apply -f tests/bats/specs/gpu-device-status-pod2.yaml
  kubectl wait --for=condition=READY pods "${_pod2}" --timeout=60s

  local after
  after=$(kubectl get resourceclaim "${_claim}" -o json | jq -c -S '.status.devices')
  assert_equal "${after}" "${before}"

  # Unprepare: once the last consumer is gone the entry is pruned, either by
  # the plugin (Unprepare) or by the API server when the claim is deallocated.
  kubectl delete pods "${_pod1}" "${_pod2}"
  kubectl wait --for=delete pods "${_pod1}" --timeout=60s
  kubectl wait --for=delete pods "${_pod2}" --timeout=60s
  if ! wait_for_device_status_pruned 180 "${_claim}"; then
    echo "status.devices was not pruned after unprepare/deallocation"
    kubectl get resourceclaim "${_claim}" -o yaml
    return 1
  fi

  # The pods are already gone; only the standalone claim is left.
  kubectl delete resourceclaim "${_claim}"
}
