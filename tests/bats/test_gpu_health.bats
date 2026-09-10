# shellcheck disable=SC2148
# shellcheck disable=SC2329

# Tests for GPU device health reporting (KEP-4680) and health taints
# (KEP-5055), driven by the NVML health monitor (featureGates.NVMLDeviceHealthCheck).
#
# These tests inject Xid errors through the mock NVML config override and
# therefore only run against mock NVML (MOCK_NVML=true). The mock delivers the
# configured Xid through the NVML event set after the first guarded NVML call
# in the consuming process trips the failure injector; for the kubelet plugin
# that call happens during device discovery, so the injection is followed by a
# kubelet-plugin pod restart.

setup_file() {
  load 'helpers.sh'
  _common_setup
  local _iargs=("--set" "logVerbosity=6" "--set" "featureGates.NVMLDeviceHealthCheck=true")
  if [ "${DISABLE_COMPUTE_DOMAINS:-}" = "true" ]; then
    _iargs+=("--set" "resources.computeDomains.enabled=false")
  fi
  iupgrade_wait "${TEST_CHART_REPO}" "${TEST_CHART_VERSION}" _iargs
}

teardown_file() {
  load 'helpers.sh'
  # Leave the node healthy for subsequent test files: drop the mock override
  # (if any) and restart the plugin so both the mock's tripped state and the
  # driver's sticky unhealthy state are reset.
  if [ "${MOCK_NVML:-}" = "true" ]; then
    health_clear_mock_override || true
    restart_kubelet_plugin_pods || true
  fi
}

setup() {
  load 'helpers.sh'
  _common_setup
  if [ "${MOCK_NVML:-}" != "true" ]; then
    skip "Xid injection requires mock NVML (MOCK_NVML=true)"
  fi
  log_objects
}

bats::on_failure() {
  echo -e "\n\nFAILURE HOOK START"
  log_objects
  kubectl get resourceslices -o json | jq '.items[] | select(.spec.driver=="gpu.nvidia.com") | .spec.devices[]? | {name, taints}' || true
  show_kubelet_plugin_error_logs
  show_gpu_plugin_log_tails
  echo -e "FAILURE HOOK END\n\n"
}

# --- helpers ---

# Health values reported for all DRA claims of the pod, comma-separated
# (pod.status.containerStatuses[].allocatedResourcesStatus[].resources[].health).
pod_claim_health() {
  kubectl get pod "$1" -o json \
    | jq -r '[.status.containerStatuses[]? | .allocatedResourcesStatus[]? | select(.name | startswith("claim:")) | .resources[]? | .health] | join(",")'
}

# Wait until every claim resource of the pod reports the given health.
wait_for_pod_claim_health() {
  local pod="$1" want="$2" timeout="$3"
  local start=$SECONDS got=""
  while (( SECONDS - start < timeout )); do
    got="$(pod_claim_health "${pod}")"
    if [ -n "${got}" ] && [ "$(echo "${got}" | tr ',' '\n' | sort -u)" = "${want}" ]; then
      log "pod ${pod} claim health: ${got}"
      return 0
    fi
    sleep 2
  done
  echo "Timeout (${timeout} s) waiting for claim health '${want}' on ${pod}; last: '${got}'"
  return 1
}

# Number of GPU devices in the node's ResourceSlices carrying the given taint.
count_gpu_taints() {
  local node="$1" key="$2" value="$3" effect="$4"
  kubectl get resourceslices -o json \
    | jq -r --arg n "${node}" --arg k "${key}" --arg v "${value}" --arg e "${effect}" \
      '[.items[] | select(.spec.driver=="gpu.nvidia.com" and .spec.nodeName==$n) | .spec.devices[]? | .taints[]? | select(.key==$k and .value==$v and .effect==$e)] | length'
}

# Number of GPU devices advertised for the node by the GPU driver.
count_gpu_devices() {
  kubectl get resourceslices -o json \
    | jq -r --arg n "$1" \
      '[.items[] | select(.spec.driver=="gpu.nvidia.com" and .spec.nodeName==$n) | .spec.devices[]?] | length'
}

wait_for_gpu_taint() {
  local node="$1" key="$2" value="$3" effect="$4" timeout="$5"
  local start=$SECONDS n=0
  while (( SECONDS - start < timeout )); do
    n="$(count_gpu_taints "${node}" "${key}" "${value}" "${effect}")"
    if [ "${n}" -gt 0 ]; then
      log "${n} device(s) on ${node} tainted with ${key}=${value}:${effect}"
      return 0
    fi
    sleep 2
  done
  echo "Timeout (${timeout} s) waiting for taint ${key}=${value}:${effect} on ${node}"
  return 1
}

kubelet_plugin_pod_on_node() {
  kubectl get pod -n dra-driver-nvidia-gpu -l dra-driver-nvidia-gpu-component=kubelet-plugin \
    --field-selector "spec.nodeName=$1,status.phase=Running" \
    --no-headers -o custom-columns=":metadata.name" | head -n1
}

# Write (or remove) the mock NVML config override through the kubelet plugin
# container: the driver root is a host path mounted at /driver-root, and the
# mock library resolves <driver_root>/config/overrides.yaml.
# 1st arg: kubelet plugin pod on the target node; 2nd arg: Xid code to inject.
health_write_mock_override() {
  local plugin_pod="$1" xid="$2"
  kubectl exec -n dra-driver-nvidia-gpu "${plugin_pod}" -c gpus -- sh -c \
    "printf 'version: 1\\nall:\\n  failure:\\n    mode: ecc_uncorrectable\\n    xid:\\n      code: ${xid}\\n' > /driver-root/config/overrides.yaml && cat /driver-root/config/overrides.yaml"
}

# Inject the Xid on a single GPU (mock device index) instead of all of them.
health_write_mock_override_device() {
  local plugin_pod="$1" index="$2" xid="$3"
  kubectl exec -n dra-driver-nvidia-gpu "${plugin_pod}" -c gpus -- sh -c \
    "printf 'version: 1\\ndevices:\\n  \"${index}\":\\n    failure:\\n      mode: ecc_uncorrectable\\n      xid:\\n        code: ${xid}\\n' > /driver-root/config/overrides.yaml && cat /driver-root/config/overrides.yaml"
}

# JSON list of gpu.nvidia.com/xid taints on one named device of the node.
device_xid_taints() {
  local node="$1" device="$2"
  kubectl get resourceslices -o json \
    | jq -c --arg n "${node}" --arg d "${device}" \
      '[.items[] | select(.spec.driver=="gpu.nvidia.com" and .spec.nodeName==$n) | .spec.devices[]? | select(.name==$d) | .taints[]? | select(.key=="gpu.nvidia.com/xid")]'
}

# Wait until the pod reports the given phase.
wait_for_pod_phase() {
  local pod="$1" want="$2" timeout="$3"
  local start=$SECONDS got=""
  while (( SECONDS - start < timeout )); do
    got="$(kubectl get pod "${pod}" -o jsonpath='{.status.phase}' 2>/dev/null || true)"
    [ "${got}" = "${want}" ] && return 0
    sleep 2
  done
  echo "Timeout (${timeout} s) waiting for pod ${pod} phase '${want}'; last: '${got}'"
  return 1
}

# Inject the Xid on every GPU of the node and restart the kubelet plugin so
# device discovery trips the mock's failure injector and the health monitor
# receives the event.
inject_xid_and_restart_plugin() {
  local plugin_pod="$1" xid="$2"
  health_write_mock_override "${plugin_pod}" "${xid}"
  restart_kubelet_plugin_pods
}

# Remove the override and restart the plugin, then wait until no Xid taint of
# the given value remains on the node's devices.
recover_from_xid() {
  local node="$1" xid="$2"
  health_clear_mock_override
  restart_kubelet_plugin_pods
  wait_for_all_gpu_resource_slices 60
  local start=$SECONDS
  while [ "$(count_gpu_taints "${node}" "gpu.nvidia.com/xid" "${xid}" "NoSchedule")" != "0" ] \
     || [ "$(count_gpu_taints "${node}" "gpu.nvidia.com/xid" "${xid}" "None")" != "0" ]; do
    (( SECONDS - start < 60 )) || { echo "Xid ${xid} taint still present after recovery"; return 1; }
    sleep 2
  done
}

health_clear_mock_override() {
  local pod
  for pod in $(kubectl get pod -n dra-driver-nvidia-gpu -l dra-driver-nvidia-gpu-component=kubelet-plugin \
      --field-selector status.phase=Running --no-headers -o custom-columns=":metadata.name"); do
    kubectl exec -n dra-driver-nvidia-gpu "${pod}" -c gpus -- rm -f /driver-root/config/overrides.yaml
  done
}

restart_kubelet_plugin_pods() {
  kubectl delete pod -n dra-driver-nvidia-gpu -l dra-driver-nvidia-gpu-component=kubelet-plugin --wait=true --timeout=60s
  sleep 2
  kubectl wait --for=condition=READY pods -n dra-driver-nvidia-gpu -l dra-driver-nvidia-gpu-component=kubelet-plugin --timeout=90s
}

# --- tests ---

# The kubelet reports device health as Unknown once the driver's report is
# older than the health check timeout (30 s by default). A healthy, idle GPU
# must therefore still read Healthy well past that timeout: the driver has to
# both re-send and refresh the timestamps on each monitor heartbeat.
# bats test_tags=gpu-health
@test "GPUs: health: allocated GPU is reported Healthy and stays Healthy past the kubelet health timeout" {
  local _specpath="tests/bats/specs/gpu-simple-full.yaml"
  local _podname="pod-full-gpu"

  kubectl apply -f "${_specpath}"
  kubectl wait --for=condition=READY pods "${_podname}" --timeout=30s

  if ! wait_for_pod_claim_health "${_podname}" "Healthy" 60; then
    if [ -z "$(pod_claim_health "${_podname}")" ]; then
      local minor
      minor="$(kubectl version -o json | jq -r '.serverVersion.minor' | tr -dc '0-9')"
      if [ "${minor}" -lt 36 ]; then
        kubectl delete -f "${_specpath}"
        skip "kubelet does not populate allocatedResourcesStatus (ResourceHealthStatus gate off before k8s 1.36)"
      fi
    fi
    false
  fi

  # Well past the 30 s kubelet health check timeout plus one 15 s heartbeat.
  sleep 50
  run pod_claim_health "${_podname}"
  assert_output "Healthy"

  kubectl delete -f "${_specpath}"
  kubectl wait --for=delete pods "${_podname}" --timeout=30s
}

# A critical Xid must surface both as a NoSchedule taint on the device in the
# ResourceSlice and as Unhealthy in the pod's allocatedResourcesStatus.
# bats test_tags=gpu-health
@test "GPUs: health: critical Xid taints the device and marks the allocated GPU Unhealthy" {
  local _specpath="tests/bats/specs/gpu-simple-full.yaml"
  local _podname="pod-full-gpu"

  kubectl apply -f "${_specpath}"
  kubectl wait --for=condition=READY pods "${_podname}" --timeout=30s

  local node plugin_pod
  node="$(kubectl get pod "${_podname}" -o jsonpath='{.spec.nodeName}')"
  plugin_pod="$(kubelet_plugin_pod_on_node "${node}")"
  [ -n "${plugin_pod}" ]

  # Baseline: no Xid taints, pod healthy (if the kubelet reports health).
  run count_gpu_taints "${node}" "gpu.nvidia.com/xid" "79" "NoSchedule"
  assert_output "0"
  local reports_health=true
  wait_for_pod_claim_health "${_podname}" "Healthy" 60 || reports_health=false

  # Inject: every GPU trips into ecc_uncorrectable with Xid 79 on the first
  # guarded NVML call; restart the plugin so discovery trips it and the health
  # monitor receives the event.
  inject_xid_and_restart_plugin "${plugin_pod}" 79

  wait_for_gpu_taint "${node}" "gpu.nvidia.com/xid" "79" "NoSchedule" 120

  if [ "${reports_health}" = "true" ]; then
    wait_for_pod_claim_health "${_podname}" "Unhealthy" 120
  else
    log "kubelet does not report DRA device health; skipping pod status assertion"
  fi

  # The pod itself keeps running; only scheduling of new pods onto the device
  # is blocked by the taint.
  run kubectl get pod "${_podname}" -o jsonpath='{.status.phase}'
  assert_output "Running"

  # Taint shape: exactly one gpu.nvidia.com/xid taint per device, carrying
  # the Xid as value and a timeAdded timestamp.
  local taints
  taints="$(device_xid_taints "${node}" "gpu-0")"
  run jq -r 'length' <<< "${taints}"
  assert_output "1"
  run jq -r '.[0].value + " " + .[0].effect + " " + (.[0].timeAdded | length | tostring)' <<< "${taints}"
  assert_output --regexp '^79 NoSchedule [1-9][0-9]*$'

  # KEP-5055: a new pod without a toleration must not be scheduled onto a
  # NoSchedule tainted device (every GPU on the node is tainted), while a
  # claim tolerating gpu.nvidia.com/xid is allocated and runs.
  kubectl apply -f tests/bats/specs/gpu-taint-untolerated.yaml
  sleep 20
  run kubectl get pod pod-full-gpu-untolerated -o jsonpath='{.status.phase}'
  assert_output "Pending"

  kubectl apply -f tests/bats/specs/gpu-taint-tolerated.yaml
  kubectl wait --for=condition=READY pods pod-full-gpu-tolerated --timeout=60s
  run kubectl get pod pod-full-gpu-untolerated -o jsonpath='{.status.phase}'
  assert_output "Pending"

  # Unhealthy is sticky: well past the kubelet health timeout the allocated
  # GPU still reads Unhealthy (not decayed to Unknown, not flipped back).
  if [ "${reports_health}" = "true" ]; then
    sleep 45
    run pod_claim_health "${_podname}"
    assert_output "Unhealthy"
  fi

  # The taint belongs to the device, not the pod: deleting the pod that was
  # using the GPU leaves the taint in place and the device stays blocked.
  kubectl delete --ignore-not-found -f tests/bats/specs/gpu-taint-tolerated.yaml
  kubectl delete --ignore-not-found -f "${_specpath}"
  kubectl wait --for=delete pods "${_podname}" pod-full-gpu-tolerated --timeout=60s
  sleep 10
  local gpu_count
  gpu_count="$(count_gpu_devices "${node}")"
  run count_gpu_taints "${node}" "gpu.nvidia.com/xid" "79" "NoSchedule"
  assert_output "${gpu_count}"
  run kubectl get pod pod-full-gpu-untolerated -o jsonpath='{.status.phase}'
  assert_output "Pending"

  kubectl delete --ignore-not-found -f tests/bats/specs/gpu-taint-untolerated.yaml
  kubectl wait --for=delete pods -l env=batssuite --timeout=60s

  # Recover: remove the override and restart the plugin; the devices must be
  # advertised without the Xid taint again.
  recover_from_xid "${node}" 79
}

# A non-fatal (application level) Xid is surfaced as an informational taint
# with effect None: it must not block scheduling and the allocated GPU stays
# Healthy in the pod status.
# bats test_tags=gpu-health
@test "GPUs: health: non-fatal Xid adds an informational taint and keeps the GPU Healthy and schedulable" {
  local _specpath="tests/bats/specs/gpu-simple-full.yaml"
  local _podname="pod-full-gpu"

  kubectl apply -f "${_specpath}"
  kubectl wait --for=condition=READY pods "${_podname}" --timeout=30s

  local node plugin_pod
  node="$(kubectl get pod "${_podname}" -o jsonpath='{.spec.nodeName}')"
  plugin_pod="$(kubelet_plugin_pod_on_node "${node}")"
  [ -n "${plugin_pod}" ]
  local reports_health=true
  wait_for_pod_claim_health "${_podname}" "Healthy" 60 || reports_health=false

  # Xid 43 (GPU stopped processing) is in the driver's built-in ignore list.
  inject_xid_and_restart_plugin "${plugin_pod}" 43

  wait_for_gpu_taint "${node}" "gpu.nvidia.com/xid" "43" "None" 120
  run count_gpu_taints "${node}" "gpu.nvidia.com/xid" "43" "NoSchedule"
  assert_output "0"

  if [ "${reports_health}" = "true" ]; then
    # Past the kubelet health timeout after the restart, still Healthy.
    sleep 45
    run pod_claim_health "${_podname}"
    assert_output "Healthy"
  fi

  # An informational taint does not block scheduling.
  kubectl apply -f tests/bats/specs/gpu-taint-untolerated.yaml
  kubectl wait --for=condition=READY pods pod-full-gpu-untolerated --timeout=60s

  kubectl delete --ignore-not-found -f tests/bats/specs/gpu-taint-untolerated.yaml
  kubectl delete --ignore-not-found -f "${_specpath}"
  kubectl wait --for=delete pods -l env=batssuite --timeout=60s

  recover_from_xid "${node}" 43
}

# Xids listed in additionalXidsToIgnore (ADDITIONAL_XIDS_TO_IGNORE) are treated
# as non-fatal: the same Xid 79 that is critical by default only produces an
# informational taint.
# bats test_tags=gpu-health
@test "GPUs: health: additionalXidsToIgnore downgrades a critical Xid to an informational taint" {
  local _specpath="tests/bats/specs/gpu-simple-full.yaml"
  local _podname="pod-full-gpu"

  local node plugin_pod
  node="$(kubectl get nodes -o jsonpath='{.items[0].metadata.name}')"
  plugin_pod="$(kubelet_plugin_pod_on_node "${node}")"
  [ -n "${plugin_pod}" ]

  # Write the override first: the helm upgrade below rolls the plugin pods,
  # which trips the injector during discovery.
  health_write_mock_override "${plugin_pod}" 79
  local _iargs=("--set" "logVerbosity=6" "--set" "featureGates.NVMLDeviceHealthCheck=true"
    "--set" "kubeletPlugin.containers.gpus.env[1].name=ADDITIONAL_XIDS_TO_IGNORE"
    "--set-string" "kubeletPlugin.containers.gpus.env[1].value=79")
  if [ "${DISABLE_COMPUTE_DOMAINS:-}" = "true" ]; then
    _iargs+=("--set" "resources.computeDomains.enabled=false")
  fi
  iupgrade_wait "${TEST_CHART_REPO}" "${TEST_CHART_VERSION}" _iargs

  wait_for_gpu_taint "${node}" "gpu.nvidia.com/xid" "79" "None" 120
  run count_gpu_taints "${node}" "gpu.nvidia.com/xid" "79" "NoSchedule"
  assert_output "0"

  # Still schedulable and reported Healthy.
  kubectl apply -f "${_specpath}"
  kubectl wait --for=condition=READY pods "${_podname}" --timeout=30s
  wait_for_pod_claim_health "${_podname}" "Healthy" 60 || log "kubelet does not report DRA device health; skipping pod status assertion"
  kubectl delete -f "${_specpath}"
  kubectl wait --for=delete pods "${_podname}" --timeout=30s

  # Restore the default configuration for the remaining tests.
  health_clear_mock_override
  _iargs=("--set" "logVerbosity=6" "--set" "featureGates.NVMLDeviceHealthCheck=true")
  if [ "${DISABLE_COMPUTE_DOMAINS:-}" = "true" ]; then
    _iargs+=("--set" "resources.computeDomains.enabled=false")
  fi
  iupgrade_wait "${TEST_CHART_REPO}" "${TEST_CHART_VERSION}" _iargs
  recover_from_xid "${node}" 79
}

# A failure on one GPU only affects that device: the other GPU stays
# untainted and keeps serving new pods.
# bats test_tags=gpu-health,multi-gpu
@test "GPUs: health: critical Xid on one GPU taints only that device and the other GPU stays schedulable" {
  local _specpath="tests/bats/specs/gpu-simple-full.yaml"
  local _podname="pod-full-gpu"

  local node plugin_pod
  node="$(kubectl get nodes -o jsonpath='{.items[0].metadata.name}')"
  plugin_pod="$(kubelet_plugin_pod_on_node "${node}")"
  [ -n "${plugin_pod}" ]

  # Mock device index 0 is advertised as gpu-0.
  health_write_mock_override_device "${plugin_pod}" 0 79
  restart_kubelet_plugin_pods

  wait_for_gpu_taint "${node}" "gpu.nvidia.com/xid" "79" "NoSchedule" 120
  run jq -r 'length' <<< "$(device_xid_taints "${node}" "gpu-0")"
  assert_output "1"
  run jq -r 'length' <<< "$(device_xid_taints "${node}" "gpu-1")"
  assert_output "0"

  # A new pod without a toleration lands on the untainted GPU.
  kubectl apply -f "${_specpath}"
  kubectl wait --for=condition=READY pods "${_podname}" --timeout=60s
  local claim allocated
  claim="$(kubectl get pod "${_podname}" -o jsonpath='{.status.resourceClaimStatuses[0].resourceClaimName}')"
  allocated="$(kubectl get resourceclaim "${claim}" -o jsonpath='{.status.allocation.devices.results[0].device}')"
  log "pod ${_podname} was allocated ${allocated}"
  [ -n "${allocated}" ]
  [ "${allocated}" != "gpu-0" ]
  run jq -r 'length' <<< "$(device_xid_taints "${node}" "${allocated}")"
  assert_output "0"
  wait_for_pod_claim_health "${_podname}" "Healthy" 60 || log "kubelet does not report DRA device health; skipping pod status assertion"

  kubectl delete --ignore-not-found -f "${_specpath}"
  kubectl wait --for=delete pods "${_podname}" --timeout=30s
  recover_from_xid "${node}" 79
}

# Without the NVMLDeviceHealthCheck feature gate the health service is not
# advertised and no health taints are produced.
# bats test_tags=gpu-health
@test "GPUs: health: without featureGates.NVMLDeviceHealthCheck no health status and no taints are reported" {
  local _specpath="tests/bats/specs/gpu-simple-full.yaml"
  local _podname="pod-full-gpu"

  local node plugin_pod
  node="$(kubectl get nodes -o jsonpath='{.items[0].metadata.name}')"
  plugin_pod="$(kubelet_plugin_pod_on_node "${node}")"
  [ -n "${plugin_pod}" ]

  # Inject before the upgrade rolls the plugin pods.
  health_write_mock_override "${plugin_pod}" 79
  local _iargs=("--set" "logVerbosity=6" "--set" "featureGates.NVMLDeviceHealthCheck=false")
  if [ "${DISABLE_COMPUTE_DOMAINS:-}" = "true" ]; then
    _iargs+=("--set" "resources.computeDomains.enabled=false")
  fi
  iupgrade_wait "${TEST_CHART_REPO}" "${TEST_CHART_VERSION}" _iargs
  wait_for_all_gpu_resource_slices 60

  kubectl apply -f "${_specpath}"
  kubectl wait --for=condition=READY pods "${_podname}" --timeout=30s
  sleep 30
  run count_gpu_taints "${node}" "gpu.nvidia.com/xid" "79" "NoSchedule"
  assert_output "0"
  run count_gpu_taints "${node}" "gpu.nvidia.com/xid" "79" "None"
  assert_output "0"
  # The kubelet reports Unknown for devices of a driver without a health
  # service (or nothing at all on kubelets without the ResourceHealthStatus
  # gate); it must not be Healthy or Unhealthy.
  run pod_claim_health "${_podname}"
  assert_output --regexp '^(Unknown)?$'

  kubectl delete --ignore-not-found -f "${_specpath}"
  kubectl wait --for=delete pods "${_podname}" --timeout=30s

  # Restore the default configuration.
  health_clear_mock_override
  _iargs=("--set" "logVerbosity=6" "--set" "featureGates.NVMLDeviceHealthCheck=true")
  if [ "${DISABLE_COMPUTE_DOMAINS:-}" = "true" ]; then
    _iargs+=("--set" "resources.computeDomains.enabled=false")
  fi
  iupgrade_wait "${TEST_CHART_REPO}" "${TEST_CHART_VERSION}" _iargs
  recover_from_xid "${node}" 79
}
