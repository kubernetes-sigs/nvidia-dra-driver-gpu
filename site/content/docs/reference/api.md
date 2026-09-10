---
title: API reference
linkTitle: API reference
weight: 10
description: Types and fields for the resource.nvidia.com/v1beta1 API group.
---

All types belong to API group `resource.nvidia.com/v1beta1`.

## GPU opaque configuration types

These types are set as opaque configuration in `ResourceClaim` and `ResourceClaimTemplate` specs to configure GPU resources. They are optional; omit them to use driver defaults. Some fields require feature gates to be enabled.

### GpuConfig

Configures a full GPU device. 
Target DeviceClass: `gpu.nvidia.com`.

With time-slicing:

```yaml
apiVersion: resource.nvidia.com/v1beta1
kind: GpuConfig
sharing:
  strategy: TimeSlicing
  timeSlicingConfig:
    interval: Default       # Default | Short | Medium | Long
```

With MPS (Multi-Process Service):

```yaml
apiVersion: resource.nvidia.com/v1beta1
kind: GpuConfig
sharing:
  strategy: MPS
  mpsConfig:
    defaultActiveThreadPercentage: 50             # optional, integer
    defaultPinnedDeviceMemoryLimit: "4Gi"         # optional, quantity
    multiUser: true                               # optional, default false
    defaultPerDevicePinnedMemoryLimit:            # optional, map
      "0": "2Gi"
```

#### Fields

| Field | Type | Description |
|---|---|---|
| `sharing` | object | Optional. Sharing strategy and its configuration. Omit to use the device exclusively. |
| `sharing.strategy` | string | `TimeSlicing` or `MPS` (Multi-Process Service). |
| `sharing.timeSlicingConfig.interval` | string | Time-slice duration: `Default`, `Short`, `Medium`, or `Long`. Requires the [`TimeSlicingSettings`](feature-gates.md) feature gate. |
| `sharing.mpsConfig.defaultActiveThreadPercentage` | integer | Thread percentage limit applied to all processes sharing the GPU. Requires the [`MPSSupport`](feature-gates.md) feature gate. |
| `sharing.mpsConfig.defaultPinnedDeviceMemoryLimit` | quantity | Pinned memory limit applied to all devices. |
| `sharing.mpsConfig.defaultPerDevicePinnedMemoryLimit` | map | Per-device override of `defaultPinnedDeviceMemoryLimit`. Keys are device index (integer) or UUID string. |
| `sharing.mpsConfig.multiUser` | bool | Starts the MPS control daemon in multi-user mode so processes with different user IDs can share the device, defaults to `false` when omitted, and requires a Volta or newer GPU. |


### MigDeviceConfig

Configures a MIG device slice. Target DeviceClass: `mig.nvidia.com`.

With time-slicing:

```yaml
apiVersion: resource.nvidia.com/v1beta1
kind: MigDeviceConfig
sharing:
  strategy: TimeSlicing
```

With MPS:

```yaml
apiVersion: resource.nvidia.com/v1beta1
kind: MigDeviceConfig
sharing:
  strategy: MPS
  mpsConfig:
    defaultActiveThreadPercentage: 50
    multiUser: true
```

#### Fields

| Field | Type | Description |
|---|---|---|
| `sharing` | object | Optional. Supports `TimeSlicing` and `MPS` strategies. |
| `sharing.strategy` | string | `TimeSlicing` or `MPS`. |
| `sharing.mpsConfig` | object | Supports the same MPS fields, defaults, and Volta-or-newer `multiUser` requirement as `GpuConfig.sharing.mpsConfig` and requires [`MPSSupport`](feature-gates.md). |

{{% alert title="Note" %}}
`timeSlicingConfig` is not available for MIG devices. Time-slicing on MIG slices does not support interval configuration.
{{% /alert %}}


### VfioDeviceConfig

Configures a VFIO passthrough device. Target DeviceClass: `vfio.gpu.nvidia.com`.

```yaml
apiVersion: resource.nvidia.com/v1beta1
kind: VfioDeviceConfig
iommu:
  backendPolicy: LegacyOnly   # LegacyOnly | PreferIommuFD
  enableAPIDevice: false       # optional
```

#### Fields

| Field | Type | Description |
|---|---|---|
| `iommu` | object | Optional. IOMMU backend configuration. Omit to use defaults (`LegacyOnly`, API device disabled). |
| `iommu.backendPolicy` | string | `LegacyOnly` (default) or `PreferIommuFD`. Selects the IOMMU backend used for device passthrough. |
| `iommu.enableAPIDevice` | bool | Optional. Expose `/dev/iommu` or `/dev/vfio/vfio` to the workload. Defaults to `false`. Requires the [`DeviceMetadata`](feature-gates.md) feature gate. |

{{% alert title="Note" %}}
Requires [`PassthroughSupport`](feature-gates.md) (Alpha, default: false). `iommu.enableAPIDevice` additionally requires [`DeviceMetadata`](feature-gates.md) (Alpha, default: false).
{{% /alert %}}

### LocalIPCConfig

Enables a writable directory scoped to an entire ResourceClaim for same-node IPC. It must not specify `requests`: every container consuming any device from the ResourceClaim receives the same directory.

```yaml
apiVersion: resource.nvidia.com/v1beta1
kind: LocalIPCConfig
enabled: true
```

#### Fields

| Field | Type | Description |
|---|---|---|
| `enabled` | bool | When `true`, exposes the directory at `/run/nvidia-dra/local-ipc`. Defaults to `false` and requires the [`LocalIPCDirectory`](feature-gates.md) feature gate. |

The driver injects the directory through CDI and sets `NVIDIA_DRA_LOCAL_IPC_DIR=/run/nvidia-dra/local-ipc`. Workload Pods do not need `hostPath` or `hostIPC` for this rendezvous. The driver provides only the directory; applications own their socket protocol, file permissions, and transferred handles.

When claim-wide configurations are provided by both a DeviceClass and a ResourceClaim, the ResourceClaim configuration takes precedence.

When the feature gate is disabled, new preparations with `enabled: true` are rejected. The driver continues to restore and remove directories for claims already in `PrepareCompleted` until they are unprepared.

## ComputeDomain CRDs

ComputeDomains use two Custom Resource Definitions. Unlike the GPU opaque types above, these are concrete Kubernetes resources that exist independently of `ResourceClaim` specs.

### ComputeDomain

A user-created resource that provisions an ephemeral multi-node NVLink fabric. Creating one triggers the controller to deploy a per-domain IMEX daemon fleet and generate `ResourceClaimTemplate` objects that workload pods reference to claim IMEX channels. The spec is immutable after creation.

```yaml
apiVersion: resource.nvidia.com/v1beta1
kind: ComputeDomain
metadata:
  name: my-compute-domain
spec:
  numNodes: 0
  channel:
    resourceClaimTemplate:
      name: my-channel
    allocationMode: Single        # Single (default) | All
```

#### Spec fields

| Field | Type | Default | Description |
|---|---|---|---|
| `spec.numNodes` | integer | — | Deprecated. Set to `0` when using the default [`IMEXDaemonsWithDNSNames`](feature-gates.md) feature gate. Formerly used to gate workload startup until a specific number of IMEX daemons joined. |
| `spec.channel.resourceClaimTemplate.name` | string | — | Name of the `ResourceClaimTemplate` the controller creates for workload pods to claim a channel from. |
| `spec.channel.allocationMode` | string | `Single` | `Single` allocates one IMEX channel per claim. `All` allocates the maximum number of channels available in the IMEX domain. |

#### Status fields

| Field | Type | Description |
|---|---|---|
| `status.status` | string | `Ready` when all expected IMEX daemons have joined; `NotReady` otherwise. |
| `status.nodes[].name` | string | Node name. |
| `status.nodes[].ipAddress` | string | Node IP used by the IMEX daemon. |
| `status.nodes[].cliqueID` | string | NVLink clique identifier for the node. |
| `status.nodes[].index` | integer | Deterministic index for the node within its clique. Used to map IPs to DNS names. |
| `status.nodes[].status` | string | Per-node daemon readiness: `Ready` or `NotReady`. |

{{% alert title="Note" %}}
Do not gate workload startup on `status.status`. The status field is informational. IMEX daemons start as soon as their local node joins without waiting for all peers.
{{% /alert %}}


### ComputeDomainClique

A `ComputeDomainClique` is a driver-managed resource created automatically by the IMEX daemon on each node. You do not create or modify these directly.

Each `ComputeDomainClique` is namespaced to the driver namespace and named as `<computeDomainUID>.<cliqueID>`. It tracks which nodes have joined a specific NVLink clique within a `ComputeDomain` and their daemon readiness.

The ComputeDomain plugin reads these objects to verify that the local IMEX daemon is ready before allowing a workload container to start.

{{% alert title="Note" %}}
Requires [`ComputeDomainCliques`](feature-gates.md) (Beta, default: true), which depends on [`IMEXDaemonsWithDNSNames`](feature-gates.md).
{{% /alert %}}

#### Fields

| Field | Type | Description |
|---|---|---|
| `daemons[].nodeName` | string | Node name. |
| `daemons[].ipAddress` | string | Node IP used by the IMEX daemon. |
| `daemons[].cliqueID` | string | NVLink clique identifier. |
| `daemons[].index` | integer | Deterministic index for the node within its clique. Maps to a DNS name. |
| `daemons[].status` | string | Daemon readiness: `Ready` or `NotReady`. Defaults to `NotReady`. |


## ComputeDomain opaque configuration types

The following opaque types are set in the `ResourceClaim` specs generated by the ComputeDomain controller. They are managed automatically, you do not set these directly. They are documented here for reference and debugging.

### ComputeDomainChannelConfig

Opaque configuration for IMEX channel `ResourceClaim` objects. Set by the controller in the `ResourceClaimTemplate` it generates from `spec.channel.resourceClaimTemplate`.

| Field | Type | Description |
|---|---|---|
| `domainID` | string | UID of the parent `ComputeDomain` resource. |
| `allocationMode` | string | Inherited from `spec.channel.allocationMode`. `Single` or `All`. May be absent when `Single` (the default). |

### ComputeDomainDaemonConfig

Opaque configuration for IMEX daemon `ResourceClaim` objects. Set by the controller in the daemon DaemonSet.

| Field | Type | Description |
|---|---|---|
| `domainID` | string | UID of the parent `ComputeDomain` resource. |
