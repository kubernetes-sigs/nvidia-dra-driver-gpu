---
title: GPU health checking
linkTitle: GPU health checking
weight: 60
description: >
  Monitor GPU health using NVML, apply device taints to prevent new workloads
  from scheduling on unhealthy GPUs, and report the health of allocated GPUs
  in the pod status.
---

The `NVMLDeviceHealthCheck` feature gate enables continuous GPU health monitoring
through the [NVIDIA Management Library (NVML)](https://developer.nvidia.com/management-library-nvml).
When a GPU enters an error state, the driver applies a
[device taint](https://kubernetes.io/docs/concepts/scheduling-eviction/dynamic-resource-allocation/#device-taints-and-tolerations)
to the corresponding [`ResourceSlice`](https://kubernetes.io/docs/concepts/scheduling-eviction/dynamic-resource-allocation/#resourceslice),
signaling the Kubernetes scheduler to avoid placing new workloads on the affected
device.

With this feature enabled, unhealthy devices are tainted in the
`ResourceSlice` so the scheduler stops placing new workloads on them, and the
health of every GPU allocated to a pod is reported to the kubelet, which
surfaces it in the pod status.

## Feature status

`NVMLDeviceHealthCheck` is an Alpha feature gate, disabled by default.

| Feature gate | Default | Stage | Since |
|---|---|---|---|
| `NVMLDeviceHealthCheck` | `false` | Alpha | v0.4.0 |

`NVMLDeviceHealthCheck` is mutually exclusive with the `PassthroughSupport`
feature gate.
Refer to the [feature gate constraints](../reference/feature-gates/#constraints) documentation for more details.

## Prerequisites

- The [`DRADeviceTaints`](https://kubernetes.io/docs/concepts/scheduling-eviction/dynamic-resource-allocation/#device-taints-and-tolerations)
  Kubernetes feature gate must be enabled on the `kube-apiserver`,
  `kube-controller-manager`, and `kube-scheduler`.
  In Kubernetes v1.34 and 1.35, `DRADeviceTaints` is disabled by default and must be explicitly enabled.
  In Kubernetes v1.36, the feature gate is enabled by default.
- For device health in the pod status, the
  [`ResourceHealthStatus`](https://kubernetes.io/docs/reference/command-line-tools-reference/feature-gates/)
  Kubernetes feature gate must be enabled on the kubelet.
  In Kubernetes v1.31 to v1.35, `ResourceHealthStatus` is disabled by default and must be explicitly enabled.
  In Kubernetes v1.36, the feature gate is enabled by default.
  Without it, the driver still taints unhealthy devices, but the pod status does not report device health.
- NVIDIA DRA driver v0.4.0 or later installed via Helm.

## How it works

When enabled, the GPU kubelet plugin starts an
[NVML event monitor](https://docs.nvidia.com/deploy/nvml-api/group__nvmlEvents.html).
When a health
event occurs on a GPU, the driver updates the `ResourceSlice` for the affected device
with a device taint.

The monitor tracks three event categories:

| Event | Taint key | Default effect | Description |
|---|---|---|---|
| XID error (fatal) | `gpu.nvidia.com/xid` | `NoSchedule` | A critical GPU hardware or firmware error. |
| XID error (non-fatal) | `gpu.nvidia.com/xid` | `None` | An application-level error that does not indicate hardware degradation. |
| GPU lost | `gpu.nvidia.com/gpu-lost` | `NoSchedule` | The GPU has become inaccessible to the driver. |
| Unmonitored | `gpu.nvidia.com/unmonitored` | `None` | The device cannot be monitored by NVML. |

### Taint effects

The driver applies the following [Kubernetes device taint effects](https://kubernetes.io/docs/concepts/scheduling-eviction/dynamic-resource-allocation/#device-taints-and-tolerations):

- `NoSchedule`: The Kubernetes scheduler does not allocate the device to new
  workloads. Existing workloads that already hold a claim to the device are not
  evicted.
- `None`: This taint is informational only. Scheduling is not affected, but the taint is
  visible in the `ResourceSlice`.

The driver only applies the `None` and `NoSchedule` effects.
The driver never evicts running workloads with `NoExecute`.
If you want to implement eviction rules when a device is tainted, create a `DeviceTaintRule` with `effect: NoExecute`.
Follow the Kubernetes documentation for [taints set by an admin](https://kubernetes.io/docs/concepts/scheduling-eviction/dynamic-resource-allocation/#taints-set-by-an-admin) for details.

### XID errors

XID codes are NVIDIA-defined error identifiers for GPU hardware and firmware
conditions.
The driver classifies XIDs as fatal or non-fatal.
Fatal XIDs produce a `NoSchedule` taint and non-fatal XIDs produce a `None` taint.
Refer to the [NVIDIA XID Errors documentation](https://docs.nvidia.com/deploy/xid-errors/latest/introduction.html) for information about XID errors and codes.

By default, the driver classifies the following XID errors as non-fatal because they indicate application-level failures rather than hardware degradation.
The following table identifies these errors:

| Code | Description |
| ---- | ----------- |
| 13 | Graphics Engine Exception |
| 31 | GPU memory page fault |
| 43 | GPU stopped processing |
| 45 | Preemptive cleanup, due to previous errors |
| 68 | Video processor exception |
| 109 | Context Switch Timeout Error |

You can classify additional XID errors as non-fatal by specifying a comma-separated list in the `--additional-xids-to-ignore` CLI argument or the `ADDITIONAL_XIDS_TO_IGNORE` environment variable.

## Device health in the pod status

The GPU kubelet plugin also reports the health of its devices to the kubelet
through the DRA device health API
([KEP-4680](https://github.com/kubernetes/enhancements/tree/master/keps/sig-node/4680-dra-resource-health)).
The kubelet shows the health of every device allocated to a container in
`pod.status.containerStatuses[].allocatedResourcesStatus`, so a workload owner
can see that the GPU their pod is running on failed without access to the
`ResourceSlice`.

The reported health is derived from the same device taints described above, so
the pod status and the `ResourceSlice` never disagree:

| Device taints | Reported health | Message |
|---|---|---|
| `gpu.nvidia.com/xid` with effect `NoSchedule` | `Unhealthy` | `critical XID <code> reported by NVML` |
| `gpu.nvidia.com/gpu-lost` | `Unhealthy` | `GPU is lost` |
| `gpu.nvidia.com/unmonitored` | `Unknown` | `device health is not monitored` |
| `gpu.nvidia.com/xid` with effect `None` | `Healthy` | `non-fatal XID <code> reported by NVML` |
| No health taints | `Healthy` | |

Once a device is `Unhealthy`, it stays `Unhealthy` until the taint is cleared
(see [Recovering from an unhealthy device](#recovering-from-an-unhealthy-device)).
A later non-fatal XID does not mask an unrecovered critical failure.

The driver re-sends the health of all devices whenever the NVML event monitor
confirms it is alive, well within the kubelet's health check timeout of 30
seconds. If the monitor stops responding, the kubelet lets the reported health
expire and shows the devices as `Unknown` rather than trusting stale data.

To view the health of the GPUs allocated to a pod:

```bash
kubectl get pod <pod> -o jsonpath='{.status.containerStatuses[*].allocatedResourcesStatus}' | jq
```

The following output shows a pod whose GPU reported a fatal XID:

```json
[
  {
    "name": "claim:gpu",
    "resources": [
      {
        "health": "Unhealthy",
        "message": "critical XID 79 reported by NVML",
        "resourceID": "k8s.gpu.nvidia.com/claim=a85e5873-fc49-4554-a151-de159f528a04-gpu-0"
      }
    ]
  }
]
```

The `name` field identifies the pod's resource claim and the `resourceID`
field identifies the allocated device. The `message` field requires the
`ResourceHealthStatusMessage` kubelet feature gate, enabled by default in
Kubernetes v1.36.

## Enabling the feature

Add the following to your Helm values:

```yaml
featureGates:
  NVMLDeviceHealthCheck: true
```

Then apply the change with `helm upgrade`:

```bash
helm upgrade dra-driver-nvidia-gpu oci://registry.k8s.io/dra-driver-nvidia/charts/dra-driver-nvidia-gpu \
    --namespace dra-driver-nvidia-gpu \
    --reuse-values \
    --set featureGates.NVMLDeviceHealthCheck=true
```

## View taints on ResourceSlice

Each taint appears on an individual device entry in `ResourceSlice.spec.devices`, not on the `ResourceSlice` itself.
Use `jq` to show only device entries that have taints:

```bash
kubectl get resourceslices -o json | jq '
  .items[].spec.devices[]
  | select((.taints // []) | length > 0)
  | {
      device: .name,
      taints: [.taints[] | {key, value, effect, timeAdded}]
    }
'
```

The following output shows an example non-fatal XID event:

```json
    {
  "device": "gpu-0-mig-1g12gb-19-0",
  "taints": [
    {
      "key": "gpu.nvidia.com/xid",
      "value": "43",
      "effect": "None",
      "timeAdded": "2026-07-22T02:24:46Z"
    }
  ]
}
```

The response includes the following details:
* The `device` field identifies the affected device entry in the `ResourceSlice`.
* The `key` field identifies the health event category, and `gpu.nvidia.com/xid` indicates an XID error.
* The `value` field contains the decimal XID code reported by NVML, which is `43` in this example.
* The `effect` field is `None` because the driver classifies XID `43` as non-fatal by default, so this taint records the event without preventing new allocations. For fatal XID codes, the effect is `NoSchedule`, which prevents new allocations that do not tolerate the taint.
* The `timeAdded` field records when the API server added the taint. The GPU kubelet plugin leaves this field unset when it adds or changes a taint so that the API server assigns the timestamp.

## Recovering from an unhealthy device

Device taints persist until the GPU kubelet plugin restarts. There is no
automated taint removal in the current release.

To clear taints after a hardware issue is resolved:

1. Confirm the hardware error is resolved. Use dmesg to check the kernal logs.
2. Restart the GPU kubelet plugin by rolling its `kubelet-plugin` DaemonSet:

```bash
kubectl rollout restart daemonset/dra-driver-nvidia-gpu-kubelet-plugin -n dra-driver-nvidia-gpu
```

On restart, the GPU kubelet plugin re-evaluates device health. Devices with no active NVML health events will not receive taints, and the pod status reports them as `Healthy` again.

> [!NOTE]
>
> If the underlying hardware issue persists, the taint is reapplied after restart.

Kubernetes administrators can use
[`DeviceTaintRule`](https://kubernetes.io/docs/concepts/scheduling-eviction/dynamic-resource-allocation/#taints-set-by-an-admin)
objects to manually remove or override device taints without restarting the driver.

> [!NOTE]
>
> `DeviceTaintRule` is gated separately from `DRADeviceTaints`. It requires the
> [`DRADeviceTaintRules`](https://kubernetes.io/docs/concepts/scheduling-eviction/dynamic-resource-allocation/#taints-set-by-an-admin)
> feature gate and the `resource.k8s.io/v1beta2` API.

## Limitations and considerations

- **No automated recovery**: Taint removal requires a driver restart or a manual
  `DeviceTaintRule` override. The driver does not clear taints when hardware
  recovers.
- **One taint per key per device**: Each device holds at most one taint per taint
  key. If multiple XID events occur on the same device, the most recent value is
  retained, except that a later event never weakens the taint: a device tainted
  `NoSchedule` by a fatal XID keeps that taint and XID code when a non-fatal XID
  follows.
- **Mutually exclusive feature gates**: Cannot be used with `PassthroughSupport`.
- **Publish failure handling**: If the driver fails to update the `ResourceSlice`
  after a health event (for example, due to a transient API server error), the
  failure is logged but not retried. The `ResourceSlice` may remain stale until
  the next successful publish or driver restart.
