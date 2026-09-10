---
title: KubeVirt VFIO GPU passthrough
linkTitle: KubeVirt VFIO GPU passthrough
weight: 50
aliases:
  - /docs/guides/kubevirt-vfio-gpu-passthrough/
description: Configure the DRA driver for NVIDIA GPUs for KubeVirt VFIO passthrough.
---

KubeVirt can attach VFIO GPU devices to virtual machines through Kubernetes Dynamic Resource Allocation (DRA). This guide covers the DRA Driver for NVIDIA GPUs configuration needed for KubeVirt passthrough workloads.

For KubeVirt feature gates, VM fields, and VM examples, refer to the [KubeVirt user guide](https://kubevirt.io/user-guide/).

## Feature status

This guide uses Alpha feature gates that are disabled by default:

| Feature gate | Stage | Default | Description |
|---|---|---|---|
| `PassthroughSupport` | Alpha | `false` | Enables VFIO passthrough allocation. |
| `DeviceMetadata` | Alpha | `false` | Exposes the VFIO API device required by KubeVirt. |
| `FabricManagerPartitioning` | Alpha | `false` | Optionally constrains VFIO claims to partitions managed by Fabric Manager. |

Refer to the [feature gates reference](../../reference/feature-gates.md) and [constraints](../../reference/feature-gates.md#constraints) for details.

## Prerequisites

- Meet the general driver [prerequisites](../../prerequisites.md).
- **IOMMU enabled** on GPU nodes intended for VFIO passthrough. Without IOMMU, the GPU kubelet plugin continues serving normal GPU devices but does not advertise VFIO devices.
- Enable the `PassthroughSupport` and `DeviceMetadata` feature gates in the DRA Driver.
- Use **KubeVirt v1.8.0 or later** with the **`GPUsWithDRA`** feature gate enabled. This gate is enabled by default from KubeVirt v1.9.0, so it only needs to be set explicitly on v1.8.x.

### NVIDIA Grace VFIO module selection

The GPU kubelet plugin automatically selects the most specific VFIO module variant that matches each GPU PCI modalias.
For example, on NVIDIA Grace systems, the kubelet plugin automatically selects the `nvgrace_gpu_vfio_pci` driver.

## Limitations and considerations

- For a GPU to switch between the `nvidia` and `vfio-pci` drivers, no process on the host can have an open handle on that GPU's `/dev/nvidia*` device nodes.

  Make sure each of the following is either not using the GPU being prepared, or is configured to release it:

  | Component | Notes |
  |---|---|
  | **display-manager (Xorg)** | **Must be disabled.** |
  | **nvidia-device-plugin** | **Must be disabled.** Not supported on the same node as the DRA Driver, which already handles both container and passthrough allocation. Exclude it from these nodes with a node selector or taint. |
  | **nvidia-persistenced** | On DRA Driver v0.4.0 or later, the DRA Driver automatically disables persistence mode on the target GPU before rebinding it to `vfio-pci` and restores it afterward, so the service can keep running. Older versions don't have this handling and require stopping the service manually. |
  | **dcgm** | Recommended to disable this service. **Note:** DCGM v4.5.0+ can automatically release GPUs when switching between the `nvidia` and `vfio-pci` drivers. |
  | **dcgm-exporter** | Recommended to disable this service. **Note:** dcgm-exporter v4.5.0+ has an experimental feature, `DCGM_EXPORTER_ENABLE_GPU_BIND_UNBIND_WATCH=true` (plus a poll frequency `DCGM_EXPORTER_GPU_BIND_UNBIND_POLL_INTERVAL=1s`), to automatically release GPUs when switching between the `nvidia` and `vfio-pci` drivers. Use with caution. |

- With `PassthroughSupport` enabled, the driver initially advertises both a GPU device and a VFIO device for each eligible physical GPU.
  These are two representations of the same hardware and cannot be used simultaneously.

  A race exists between claim allocation and device preparation.
  If a container GPU claim and a VFIO passthrough claim are allocated for the same physical GPU before either device is prepared, the scheduler can allocate both.
  Whichever device is prepared first succeeds.
  Preparation of the other fails with `allocatable not found for device "<device-name>"`.

  After device preparation succeeds, the driver removes the alternate device type from the node's `ResourceSlice` and prevents further conflicting allocations.

  One way to avoid conflicting allocations is to submit the workloads serially and waiting for the first workload's device preparation to complete before submitting the next.
  Another way to prevent conflicting allocations is to dedicate separate nodes to container GPU and VFIO workloads.
  Label the nodes by workload type, and add the matching `nodeSelector` to every GPU pod and KubeVirt VM.
  For stronger isolation, you can taint the VFIO nodes with `NoSchedule` and add the corresponding toleration only to VFIO workloads.

  For example, label the container and VFIO nodes and taint the VFIO nodes:

  ```bash
  kubectl label node <container-node> demo/gpu-workload=container
  kubectl label node <vfio-node> demo/gpu-workload=vfio
  kubectl taint node <vfio-node> demo/gpu-workload=vfio:NoSchedule
  ```

  Add the following node selector to container GPU pods:

  ```yaml
  spec:
    nodeSelector:
      demo/gpu-workload: container
  ```

  Add the matching node selector and toleration to the KubeVirt VM template:

  ```yaml
  spec:
    template:
      spec:
        nodeSelector:
          demo/gpu-workload: vfio
        tolerations:
        - key: demo/gpu-workload
          operator: Equal
          value: vfio
          effect: NoSchedule
  ```

  A toleration permits scheduling on the tainted nodes but does not require it, so use it together with the node selector.
  Ensure that the DRA kubelet plugin and other required DaemonSets also tolerate the taint.

  If a conflict occurs, delete and recreate the failed pod and its resource claim after the other workload completes device preparation.
  When the claim is generated from a `ResourceClaimTemplate`, deleting the pod also deletes the generated claim.

## Install the DRA Driver

Install the driver with passthrough support and device metadata:

```bash
helm upgrade -i dra-driver-nvidia-gpu oci://registry.k8s.io/dra-driver-nvidia/charts/dra-driver-nvidia-gpu \
    --version {{< param "driver_version" >}} \
    --namespace dra-driver-nvidia-gpu \
    --create-namespace \
    --set resources.gpus.enabled=true \
    --set resources.computeDomains.enabled=false \
    --set gpuResourcesEnabledOverride=true \
    --set nvidiaDriverRoot=/ \
    --set featureGates.PassthroughSupport=true \
    --set featureGates.DeviceMetadata=true
```

Set `nvidiaDriverRoot` based on how the NVIDIA driver is installed on your nodes:

- `/` for a host-installed driver.
- `/run/nvidia/driver` for a GPU Operator-managed driver.
- `/home/kubernetes/bin/nvidia` for a GKE-managed driver.

### Optional: Fabric Manager partitioning

On supported HGX and single-node NVL systems, Fabric Manager partitioning provides
additional isolation at the NVLink fabric level for passthrough workloads using
claims with `vfio.gpu.nvidia.com` device classes.
The partitioning also supports full-GPU claims and does not depend on `PassthroughSupport`.
This guide already enables `PassthroughSupport` because the base resource is
VFIO.

Before enabling the gate, configure Fabric Manager with `FABRIC_MODE=1` and
update every applicable VFIO claim to select one complete published partition.
Refer to [Fabric Manager partitioning](fabric-manager-partitioning.md) for
platform prerequisites, claim migration, topology inspection, and
troubleshooting.

Enable the optional gate on the existing Helm release:

```bash
helm upgrade -i dra-driver-nvidia-gpu oci://registry.k8s.io/dra-driver-nvidia/charts/dra-driver-nvidia-gpu \
    --version {{< param "driver_version" >}} \
    --namespace dra-driver-nvidia-gpu \
    --reuse-values \
    --set featureGates.FabricManagerPartitioning=true
```

With the gate enabled, VFIO ResourceSlice devices publish `gpuModuleID` and
the `partitionN` attributes reported for each physical GPU.
The GPU kubelet plugin activates the exact partition before binding the
selected GPUs to VFIO and deactivates it after the GPUs return to the NVIDIA
driver.
For the attribute definitions, refer to
[ResourceSlice device attributes](../../reference/resourceslice-attributes.md#fabric-manager-partition-attributes).

Verify that the driver registered the expected `DeviceClass` and advertised node resources:

```bash
kubectl get deviceclass
```

Example output:

```
NAME                  AGE
gpu.nvidia.com        25s
mig.nvidia.com        25s
vfio.gpu.nvidia.com   25s
```

```bash
kubectl get resourceslice
```

Example output:

```
NAME                                                            NODE                                   DRIVER           POOL                                   AGE
00000-gpu.nvidia.com-dra-driver-nvidia-gpu-cluster-worker-tr5pp dra-driver-nvidia-gpu-cluster-worker gpu.nvidia.com   dra-driver-nvidia-gpu-cluster-worker   3m17s
```

VFIO devices carry the `gpu.nvidia.com/type` attribute value `vfio`. For the VFIO
device attributes you can match with CEL selectors, refer to
[ResourceSlice device attributes](../../reference/resourceslice-attributes.md#vfio-passthrough-type-vfio).
To inspect ResourceSlice output on your cluster, refer to
[View available GPU resources](view-resources.md).

## VFIO passthrough claim template

For KubeVirt GPU passthrough, create a `ResourceClaimTemplate` that uses the `vfio.gpu.nvidia.com` `DeviceClass`. A `ResourceClaimTemplate` is namespace-scoped and defines the GPU request and its configuration. Multiple pods can reuse the same template: Kubernetes automatically creates one `ResourceClaim` per pod from it, and deletes that claim when the pod terminates.

### Creating the claim template

```yaml
apiVersion: resource.k8s.io/v1
kind: ResourceClaimTemplate
metadata:
  name: dra-gpu-claim-template
spec:
  spec:
    devices:
      config:
      - requests:
        - dra-gpu
        opaque:
          driver: gpu.nvidia.com
          parameters:
            apiVersion: resource.nvidia.com/v1beta1
            kind: VfioDeviceConfig
            iommu:
              backendPolicy: LegacyOnly
              enableAPIDevice: true
      requests:
      - name: dra-gpu
        exactly:
          allocationMode: ExactCount
          count: 1
          deviceClassName: vfio.gpu.nvidia.com
```

Save it as `vfio-gpu-rct.yaml`.

### Apply the manifest

```bash
kubectl apply -f vfio-gpu-rct.yaml
```

Example output:

```
resourceclaimtemplate.resource.k8s.io/dra-gpu-claim-template created
```

### VfioDeviceConfig parameters

The opaque `VfioDeviceConfig` block tells the DRA Driver which VFIO device nodes to mount into virt-launcher through CDI.

- **`enableAPIDevice: true`** — Mounts the VFIO control device `/dev/vfio/vfio` into the virt-launcher pod. KubeVirt **requires** this device to manage VFIO PCI assignments through libvirt.

- **`backendPolicy: LegacyOnly`** — Selects the legacy IOMMU VFIO backend (`/dev/vfio/<iommu-group>`). The alternative, `PreferIommuFD`, uses the IOMMUFD backend (`/dev/vfio/devices/vfio*`) when available on the host.

Keep `backendPolicy: LegacyOnly` for KubeVirt, which does not support the IOMMUFD backend yet.

### Select a Fabric Manager partition

For a two-GPU VM on a Fabric Manager node, change the request count to `2` and
require both GPUs to share a size-two partition:

```yaml
devices:
  requests:
  - name: dra-gpu
    exactly:
      allocationMode: ExactCount
      count: 2
      deviceClassName: vfio.gpu.nvidia.com
  constraints:
  - requests:
    - dra-gpu
    matchAttribute: gpu.nvidia.com/partition2
```

The scheduler selects two VFIO GPUs with the same `partition2` value.
Claim preparation fails if the selected GPU set does not exactly match a
partition reported by Fabric Manager.
Keep a single `VfioDeviceConfig` block for all VFIO requests in the claim.
For why the count and constraint must be used together, refer to
[Request a VFIO partition](fabric-manager-partitioning.md#request-a-vfio-partition).

## Troubleshooting

If a VM fails to start because its virt-launcher pod is stuck in `ContainerCreating` state, check the kubelet-plugin logs for prepare errors:

```bash
kubectl logs -n dra-driver-nvidia-gpu -l dra-driver-nvidia-gpu-component=kubelet-plugin -c gpus
```

A `NodePrepareResources` failure looks something like this:

```
Warning  FailedPrepareDynamicResources  22s   kubelet  Failed to prepare dynamic resources: prepare dynamic resources: NodePrepareResources: rpc error: code = DeadlineExceeded desc = context deadline exceeded
```

A `DeadlineExceeded` error for `NodePrepareResources` for the workload usually means a service still holds an open handle to the GPU blocking its driver switch. On the GPU node, the culprit process can be identified using:

```bash
for f in /proc/[0-9]*/fd/*; do t=$(readlink "$f" 2>/dev/null) || continue; case "$t" in /dev/nvidia[0-9]*) echo "PID $(echo "$f" | cut -d/ -f3) holds $t";; esac; done
```

Example output:

```
PID 1210233 holds /run/nvidia/driver/dev/nvidia0
PID 1237013 holds /run/nvidia/driver/dev/nvidia1
```

If the command returns an output, note the PID holding the device node of the GPU being prepared, identify the owning service, and stop those services.

The subsequent invocation of `NodePrepareResources` call for the workload should then succeed, and the pod should reach the Running state.

If no process is holding the GPU and the VM still fails to start, open a bug.
