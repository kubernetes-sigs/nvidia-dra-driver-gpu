---
title: GPU sharing
linkTitle: GPU sharing
weight: 25
description: How several workloads can use one GPU, and how to choose between the two ways of setting that up.
---

Sharing lets more than one workload use a single physical GPU. By default each `ResourceClaim` gets a GPU of its own, so two pods that write their own claims land on two different GPUs.

Sharing can be arranged in two ways. With user-mediated sharing, several workloads reference one claim. With system-mediated sharing, the workloads keep independent claims and the cluster places them on the same GPU. This page explains both models and how to choose between them.

That choice is separate from how the GPU itself divides the work, which is the job of time-slicing, MPS, and MIG.

For those techniques and the resource types they apply to, see [GPU allocation](gpu-allocation.md).

---

## User-mediated sharing

Write one `ResourceClaim` and point several containers at it: they share the claim,
so they share its GPU. You control exactly who shares, but a claim belongs to a
namespace, so everyone sharing it must be in that namespace too. Set the technique
with `sharing.strategy` in the claim's `GpuConfig` or `MigDeviceConfig`.

---

## System-mediated sharing

Every pod writes its own claim, and none of them refer to each other. An
administrator turns on consumable capacity; the driver then advertises each GPU as
able to hold several claims, and the scheduler works out which ones fit together.
Authors only say how much they need, such as 4Gi of GPU memory, and their pods can
be in different namespaces because the claims are separate.

---

## Comparing the two

|  | User-mediated | System-mediated |
|---|---|---|
| Sharing across namespaces | No | Yes |
| Techniques | Time-slicing, MPS | Time-slicing only |
| Driver feature gate | `TimeSlicingSettings` or `MPSSupport` | `ConsumableShares`, plus a `consumableShares` mode |
| Kubernetes | No additional requirements | [`DRAConsumableCapacity`](https://kubernetes.io/docs/concepts/scheduling-eviction/dynamic-resource-allocation/#consumable-capacity), required on v1.34 and v1.35, on by default from v1.36 |
| Guide | [Time-slicing](../guides/gpu-allocation/time-slicing.md), [MPS](../guides/mps.md) | [Consumable capacity](../guides/gpu-allocation/consumable-capacity.md) |

All of these driver feature gates are Alpha and off by default; refer to
[Feature gates](../reference/feature-gates.md) to enable them. MPS and consumable
capacity cannot be combined — a claim asking for MPS is rejected when the pod
starts.

---

## Choosing a sharing pattern

| If your workload is... | Use | Model |
|---|---|---|
| Batch jobs, notebooks, or dev containers that sit idle between bursts | [Time-slicing](../guides/gpu-allocation/time-slicing.md) | User-mediated |
| Processes that must run at the same time, each with a limit on how much it takes | [MPS](../guides/mps.md) | User-mediated |
| Replicas that scale on their own, perhaps in different namespaces, packing onto a pool of GPUs | [Consumable capacity](../guides/gpu-allocation/consumable-capacity.md) | System-mediated |
| Tenants that must not be able to affect each other at all | [MIG](../guides/gpu-allocation/mig.md) | Either |

---

## Get started

For manifests that set up each pattern, see [`demo/specs/quickstart/`](https://github.com/kubernetes-sigs/dra-driver-nvidia-gpu/tree/{{< param driver_release_tag >}}/demo/specs/quickstart) for time-slicing and MPS, and [`demo/specs/consumable-shares/`](https://github.com/kubernetes-sigs/dra-driver-nvidia-gpu/tree/{{< param driver_release_tag >}}/demo/specs/consumable-shares) for consumable capacity.
