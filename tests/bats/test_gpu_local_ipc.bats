# shellcheck disable=SC2148
# shellcheck disable=SC2329

setup_file() {
  load 'helpers.sh'
  _common_setup
  local _iargs=("--set" "logVerbosity=6"
    "--set" "featureGates.LocalIPCDirectory=true")
  if [ "${DISABLE_COMPUTE_DOMAINS:-}" = "true" ]; then
    _iargs+=("--set" "resources.computeDomains.enabled=false")
  fi
  iupgrade_wait "${TEST_CHART_REPO}" "${TEST_CHART_VERSION}" _iargs
  kubectl delete namespace local-ipc-e2e --ignore-not-found --wait=true --timeout=120s
  kubectl create namespace local-ipc-e2e
  kubectl label namespace local-ipc-e2e pod-security.kubernetes.io/enforce=restricted --overwrite
}

teardown_file() {
  kubectl delete namespace local-ipc-e2e --ignore-not-found --wait=true --timeout=120s
}

setup() {
  load 'helpers.sh'
  _common_setup
  log_objects
}

bats::on_failure() {
  echo -e "\n\nFAILURE HOOK START"
  log_objects
  kubectl get resourceclaims -n local-ipc-e2e --ignore-not-found || true
  kubectl get pods -n local-ipc-e2e -o wide --ignore-not-found || true
  show_kubelet_plugin_error_logs
  show_gpu_plugin_log_tails
  echo -e "FAILURE HOOK END\n\n"
}

# bats test_tags=fastfeedback,gpu-local-ipc
@test "GPUs: LocalIPCDirectory shares claim-scoped files and cleans up" {
  cat <<'SPEC' | kubectl apply -n local-ipc-e2e -f -
apiVersion: resource.k8s.io/v1
kind: ResourceClaim
metadata:
  name: rc-local-ipc
  labels:
    env: batssuite
spec:
  devices:
    requests:
    - name: gpu
      exactly:
        deviceClassName: gpu.nvidia.com
    config:
    - opaque:
        driver: gpu.nvidia.com
        parameters:
          apiVersion: resource.nvidia.com/v1beta1
          kind: LocalIPCConfig
          enabled: true
---
apiVersion: v1
kind: Pod
metadata:
  name: pod-local-ipc-writer
  labels:
    env: batssuite
spec:
  restartPolicy: Never
  resourceClaims:
  - name: gpu
    resourceClaimName: rc-local-ipc
  containers:
  - name: writer
    image: ubuntu:24.04
    command: ["bash", "-lc"]
    args:
    - |
      test -n "${NVIDIA_DRA_LOCAL_IPC_DIR}"
      printf shared > "${NVIDIA_DRA_LOCAL_IPC_DIR}/value"
      chmod 0666 "${NVIDIA_DRA_LOCAL_IPC_DIR}/value"
      echo writer-ready
      sleep 300
    resources:
      claims:
      - name: gpu
        request: gpu
    securityContext:
      allowPrivilegeEscalation: false
      capabilities:
        drop: ["ALL"]
      runAsNonRoot: true
      runAsUser: 1000
      seccompProfile:
        type: RuntimeDefault
---
apiVersion: v1
kind: Pod
metadata:
  name: pod-local-ipc-reader
  labels:
    env: batssuite
spec:
  restartPolicy: Never
  resourceClaims:
  - name: gpu
    resourceClaimName: rc-local-ipc
  containers:
  - name: reader
    image: ubuntu:24.04
    command: ["bash", "-lc"]
    args:
    - |
      test -n "${NVIDIA_DRA_LOCAL_IPC_DIR}"
      sleep 300
    resources:
      claims:
      - name: gpu
        request: gpu
    securityContext:
      allowPrivilegeEscalation: false
      capabilities:
        drop: ["ALL"]
      runAsNonRoot: true
      runAsUser: 2000
      seccompProfile:
        type: RuntimeDefault
SPEC

  kubectl wait -n local-ipc-e2e --for=condition=READY pods pod-local-ipc-writer pod-local-ipc-reader --timeout=60s

  local reader_output=""
  for _ in $(seq 1 60); do
    reader_output=$(kubectl exec -n local-ipc-e2e pod-local-ipc-reader -- \
      bash -lc 'cat "${NVIDIA_DRA_LOCAL_IPC_DIR}/value"' 2>/dev/null || true)
    [ "${reader_output}" = "shared" ] && break
    sleep 1
  done
  assert_equal "${reader_output}" "shared"

  local writer_inode reader_inode
  writer_inode=$(kubectl exec -n local-ipc-e2e pod-local-ipc-writer -- \
    bash -lc 'stat -Lc "%d:%i" "${NVIDIA_DRA_LOCAL_IPC_DIR}"')
  reader_inode=$(kubectl exec -n local-ipc-e2e pod-local-ipc-reader -- \
    bash -lc 'stat -Lc "%d:%i" "${NVIDIA_DRA_LOCAL_IPC_DIR}"')
  assert_equal "${reader_inode}" "${writer_inode}"

  local unsafe_pods
  unsafe_pods=$(kubectl get pods -n local-ipc-e2e pod-local-ipc-writer pod-local-ipc-reader -o json | \
    jq '[.items[] | select(.spec.hostIPC == true or any(.spec.volumes[]?; has("hostPath")))] | length')
  assert_equal "${unsafe_pods}" "0"

  local claim_uid node
  claim_uid=$(kubectl get resourceclaim -n local-ipc-e2e rc-local-ipc -o jsonpath='{.metadata.uid}')
  node=$(kubectl get pod -n local-ipc-e2e pod-local-ipc-writer -o jsonpath='{.spec.nodeName}')
  [ -n "${claim_uid}" ]
  [ -n "${node}" ]

  kubectl delete pods -n local-ipc-e2e pod-local-ipc-writer pod-local-ipc-reader
  kubectl wait -n local-ipc-e2e --for=delete pods pod-local-ipc-writer pod-local-ipc-reader --timeout=60s
  kubectl delete resourceclaim -n local-ipc-e2e rc-local-ipc

  local plugin_pod
  plugin_pod=$(kubectl get pods -n dra-driver-nvidia-gpu \
    -l dra-driver-nvidia-gpu-component=kubelet-plugin \
    --field-selector spec.nodeName="${node}" \
    -o jsonpath='{.items[0].metadata.name}')
  [ -n "${plugin_pod}" ]

  local removed="false"
  for _ in $(seq 1 30); do
    if kubectl exec -n dra-driver-nvidia-gpu "${plugin_pod}" -c gpus -- \
      test ! -e "/var/lib/kubelet/plugins/gpu.nvidia.com/local-ipc/${claim_uid}"; then
      removed="true"
      break
    fi
    sleep 1
  done
  assert_equal "${removed}" "true"
}
