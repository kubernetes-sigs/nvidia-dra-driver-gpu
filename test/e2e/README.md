# test/e2e

End-to-end suite for `dra-driver-nvidia-gpu` using Go + Ginkgo v2. Covers nine
DRA scenarios driven by CEL selectors and embedded YAML templates.

## Coverage

| # | Test | What it validates |
|---|---|---|
| 1 | `[install]` | `gpu.nvidia.com` driver publishes a ResourceSlice with productName / driverVersion / memory. |
| 2 | `[cel/productName]` | CEL regex on `productName` schedules a pod onto a matching GPU. |
| 3 | `[cel/driverVersion]` | CEL semver `compareTo(semver(X)) >= 0` on `driverVersion`. |
| 4 | `[cel/memory]` | CEL quantity compare on `capacity.memory` at a 90% threshold. |
| 5 | `[sharing]` | N pods share a single `ResourceClaim` (time-slicing outcome). |
| 6 | `[negative]` | Unmatchable selector leaves the pod Pending with no allocation. |
| 7 | `[consumable-shares/unlimited]` | Multiple pods share the same GPU concurrently with `--consumable-shares=unlimited`. |
| 8 | `[consumable-shares/memory]` | Multiple pods with explicit fractional memory requests share the same GPU with `--consumable-shares=memory`. |
| 9 | `[device-status]` | KEP-4817: `ResourceClaim.status.devices` is published on prepare (uuid matches the ResourceSlice), untouched by a second consumer, and pruned after the last consumer goes away. Skipped unless the driver runs with `featureGates.ResourceClaimDeviceStatus=true`. |

The suite detects GPU product / driver / memory from the published
`ResourceSlice` at `BeforeSuite`, so it adapts to whatever hardware the CI
harness provisions (T4, L4, A10, A100, H100, etc.).

## Prerequisites

- Kubernetes cluster with `DynamicResourceAllocation` enabled.
- GPU Operator installed (minimal mode is fine) so node-feature-discovery
  labels the node with `nvidia.com/gpu.product`, `nvidia.com/gpu.memory`,
  etc.
- DRA driver installed (`dra-driver-nvidia-gpu` chart or equivalent).
- `kubectl` on PATH; current context pointing at the target cluster.

## Running

```bash
make test-e2e                                   # from the repo root
go test -mod=vendor -v -timeout=30m ./test/e2e/... -ginkgo.v   # directly
```

`$ARTIFACTS/junit_01.xml` is produced by `-ginkgo.junit-report`, which Prow
picks up automatically when `runner.sh` sets `ARTIFACTS`.

## Layout

```
test/e2e/
├── README.md
├── suite_test.go              # Ginkgo bootstrap + GPU detection
├── gpu_allocation_test.go     # 8 It() specs
├── device_status_test.go      # [device-status] spec (KEP-4817)
└── framework/
    ├── client.go              # k8s client factory
    ├── gpu.go                 # ResourceSlice -> GPUDetails
    ├── manifests.go           # embed + render specs/*.tmpl
    ├── wait.go                # pod/claim polling helpers
    └── specs/                 # embedded YAML templates
        ├── consumable-shares-memory.yaml.tmpl
        ├── consumable-shares-unlimited.yaml.tmpl
        ├── device-status.yaml.tmpl
        ├── driver-version.yaml.tmpl
        ├── error-handling.yaml.tmpl
        ├── memory-size.yaml.tmpl
        ├── product-type.yaml.tmpl
        └── timeslicing.yaml.tmpl
```
