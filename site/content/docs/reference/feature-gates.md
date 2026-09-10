---
title: Feature gates
linkTitle: Feature gates
weight: 20
description: Available feature gates, their stages, defaults, and constraints.
---

Feature gates control experimental and beta functionality in the DRA Driver for NVIDIA GPUs. They follow [Kubernetes feature gate conventions](https://kubernetes.io/docs/reference/command-line-tools-reference/feature-gates/).

## Set feature gates

Set feature gates in your Helm values file:

```yaml
featureGates:
  TimeSlicingSettings: true
  MPSSupport: true
```

Or pass them at install time:

```bash
helm install dra-driver-nvidia-gpu oci://registry.k8s.io/dra-driver-nvidia/charts/dra-driver-nvidia-gpu \
    --version {{< param "driver_version" >}} \
    --namespace dra-driver-nvidia-gpu \
    --set "featureGates.TimeSlicingSettings=true"
```

## Available feature gates

The Kubernetes column identifies additional Kubernetes version or Kubernetes feature gate requirements beyond the baseline versions in [Prerequisites](../prerequisites.md).

| Feature gate | Stage | Default | Kubernetes | Description |
| --- | --- | --- | --- | --- |
| `TimeSlicingSettings` | Alpha | `false` | No requirements. | Enables customization of CUDA time-slicing settings in `GpuConfig`. |
| `MPSSupport` | Alpha | `false` | No requirements. | Enables Multi-Process Service (MPS) sharing strategy in `GpuConfig` and `MigDeviceConfig`. |
| `IMEXDaemonsWithDNSNames` | Beta | `true` | No requirements. | IMEX daemons use DNS names instead of raw IP addresses for peer communication. Required by `ComputeDomainCliques`. |
| `PassthroughSupport` | Alpha | `false` | No requirements. | Enables VFIO passthrough device allocation using `VfioDeviceConfig`. |
| `DynamicMIG` | Alpha | `false` | v1.34 through v1.35 requires `DRAPartitionableDevices=true` on the kube-apiserver and kube-scheduler.  v1.36 and later enables `DRAPartitionableDevices` by default. See [Kubernetes feature gates](https://kubernetes.io/docs/reference/command-line-tools-reference/feature-gates/#DRAPartitionableDevices). | Enables dynamic MIG device allocation and reconfiguration. See [MIG](../guides/gpu-allocation/mig.md). |
| `NVMLDeviceHealthCheck` | Alpha | `false` | No requirements. | Enables GPU health checking using NVML. |
| `ComputeDomainCliques` | Beta | `true` | No requirements. | Uses `ComputeDomainClique` CRD objects to track IMEX daemon membership. Requires `IMEXDaemonsWithDNSNames`. |
| `CrashOnNVLinkFabricErrors` | Beta | `true` | No requirements. | Causes the kubelet plugin to crash rather than fall back to non-fabric mode when NVLink fabric errors are detected. |
| `DeviceMetadata` | Alpha | `false` | No requirements. | Enables IOMMU API device exposure, such as `/dev/iommu` or `/dev/vfio/vfio`, for VFIO workloads with `VfioDeviceConfig`. Requires `PassthroughSupport`. |
| `FabricManagerPartitioning` | Alpha | `false` | No requirements. | Enables Fabric Manager partition discovery and lifecycle management for full GPUs and VFIO devices on supported HGX and single-node NVL systems. VFIO devices require `PassthroughSupport`. Requires Fabric Manager with `FABRIC_MODE=1`. Each full-GPU or VFIO claim on a participating node must exactly match one published partition. See [Fabric Manager partitioning](../guides/gpu-allocation/fabric-manager-partitioning.md). |
| `HostManagedIMEXDaemon` | Alpha | `false` | No requirements. | Allows `resources.computeDomains.imex.mode=hostManaged`, where you manage the host `nvidia-imex` service instead of the DRA Driver creating per-ComputeDomain daemon DaemonSets. This gate only unlocks the mode. Driver-managed DNS daemon naming and `ComputeDomainClique` tracking are not used in this mode. See [Validate host-managed IMEX](../guides/compute-domain-workloads.md#validate-host-managed-imex). |
| `DRAListTypeAttributes` | Alpha | `false` | v1.36 or later with `DRAListTypeAttributes=true` on the kube-apiserver and kube-scheduler. | Publishes list-valued DRA device attributes, including `resource.kubernetes.io/numaNode` as a one-element list. See [NUMA locality](resourceslice-attributes.md#numa-locality). |
| `ConsumableShares` | Alpha | `false` | v1.34 through v1.35 requires `DRAConsumableCapacity=true` on the kube-apiserver, kube-controller-manager, kube-scheduler, and kubelet; v1.36 and later enables `DRAConsumableCapacity` by default; see [Kubernetes consumable capacity](https://kubernetes.io/docs/concepts/scheduling-eviction/dynamic-resource-allocation/#consumable-capacity). | Implements Kubernetes DRA consumable capacity for full GPUs and MIG devices by publishing them as multi-allocatable and defining their capacity request policies; also set the [`consumableShares`](helm-values.md#feature-gates) Helm value to select an accounting mode; see [Consumable capacity](../guides/gpu-allocation/consumable-capacity.md). |
| `LocalIPCDirectory` | Alpha | `false` | No requirements. | Enables `LocalIPCConfig`, which provides a writable, claim-scoped directory at `/run/nvidia-dra/local-ipc` for same-node IPC. |

## Constraints

The following feature gate combinations are mutually exclusive and cannot be enabled together.

| Combination | Reason |
| --- | --- |
| `DynamicMIG` + `PassthroughSupport` | Mutually exclusive |
| `DynamicMIG` + `MPSSupport` | Mutually exclusive |
| `PassthroughSupport` + `NVMLDeviceHealthCheck` | Mutually exclusive |

The feature gates below have the following dependencies:

| Feature gate | Requires |
| --- | --- |
| `ComputeDomainCliques` | `IMEXDaemonsWithDNSNames` |
| `DeviceMetadata` | `PassthroughSupport` |
