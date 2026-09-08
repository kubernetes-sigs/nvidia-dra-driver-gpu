/*
Copyright The Kubernetes Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    https://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
	resourceapi "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
)

func deviceStatusTestState() *DeviceState {
	gpu0 := &GpuInfo{
		UUID:          "GPU-0000",
		minor:         0,
		productName:   "NVIDIA H100 80GB HBM3",
		driverVersion: "565.57.01",
		pciBusID:      "0000:01:00.0",
	}
	gpu1 := &GpuInfo{
		UUID:          "GPU-1111",
		minor:         1,
		productName:   "NVIDIA A100-SXM4-40GB",
		driverVersion: "565.57.01",
		pciBusID:      "0000:02:00.0",
	}
	return &DeviceState{
		nvdevlib: &deviceLib{
			gpuInfosByUUID: map[string]*GpuInfo{
				gpu0.UUID: gpu0,
				gpu1.UUID: gpu1,
			},
		},
		perGPUAllocatable: &PerGPUAllocatableDevices{
			allocatablesMap: map[PCIBusID]AllocatableDevices{
				"0000:01:00.0": {
					"gpu-0": &AllocatableDevice{Gpu: gpu0},
				},
				"0000:02:00.0": {
					"gpu-1-mig-1g5gb-19-0": &AllocatableDevice{
						MigStatic: &MigDeviceInfo{
							UUID:       "MIG-2222",
							Profile:    "1g.5gb",
							ParentUUID: gpu1.UUID,
						},
					},
				},
				"0000:03:00.0": {
					"gpu-vfio-0": &AllocatableDevice{
						Vfio: &VfioDeviceInfo{
							UUID:        "GPU-3333",
							index:       0,
							productName: "NVIDIA H100 80GB HBM3",
							PciBusID:    "0000:03:00.0",
						},
					},
				},
			},
		},
	}
}

func claimWithResults(results ...resourceapi.DeviceRequestAllocationResult) *resourceapi.ResourceClaim {
	return &resourceapi.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "claim-1",
			Namespace: "default",
			UID:       types.UID("uid-1"),
		},
		Status: resourceapi.ResourceClaimStatus{
			Allocation: &resourceapi.AllocationResult{
				Devices: resourceapi.DeviceAllocationResult{
					Results: results,
				},
			},
		},
	}
}

func decodeStatusData(t *testing.T, status resourceapi.AllocatedDeviceStatus) DeviceStatusData {
	t.Helper()
	require.NotNil(t, status.Data)
	var data DeviceStatusData
	require.NoError(t, json.Unmarshal(status.Data.Raw, &data))
	return data
}

func TestBuildDeviceStatuses(t *testing.T) {
	state := deviceStatusTestState()

	claim := claimWithResults(
		resourceapi.DeviceRequestAllocationResult{
			Request: "req-gpu", Driver: DriverName, Pool: "node-1", Device: "gpu-0",
		},
		resourceapi.DeviceRequestAllocationResult{
			Request: "req-mig", Driver: DriverName, Pool: "node-1", Device: "gpu-1-mig-1g5gb-19-0",
		},
		resourceapi.DeviceRequestAllocationResult{
			Request: "req-vfio", Driver: DriverName, Pool: "node-1", Device: "gpu-vfio-0",
		},
		// Allocation result owned by another driver: must be ignored.
		resourceapi.DeviceRequestAllocationResult{
			Request: "req-other", Driver: "other.example.com", Pool: "node-1", Device: "gpu-0",
		},
	)

	preparedDevices := PreparedDevices{
		&PreparedDeviceGroup{
			Devices: PreparedDeviceList{
				{
					Gpu: &PreparedGpu{
						Info:   &GpuInfo{UUID: "GPU-0000"},
						Device: &CheckpointedDevice{DeviceName: "gpu-0", PoolName: "node-1"},
					},
				},
				{
					Mig: &PreparedMigDevice{
						Concrete: &MigLiveTuple{MigUUID: "MIG-2222", ParentUUID: "GPU-1111"},
						Device:   &CheckpointedDevice{DeviceName: "gpu-1-mig-1g5gb-19-0", PoolName: "node-1"},
					},
				},
				{
					Vfio: &PreparedVfioDevice{
						Info:   &VfioDeviceInfo{UUID: "GPU-3333"},
						Device: &CheckpointedDevice{DeviceName: "gpu-vfio-0", PoolName: "node-1"},
					},
				},
			},
		},
	}

	statuses := state.buildDeviceStatuses(claim, preparedDevices)
	require.Len(t, statuses, 3)

	require.Equal(t, DriverName, statuses[0].Driver)
	require.Equal(t, "node-1", statuses[0].Pool)
	require.Equal(t, "gpu-0", statuses[0].Device)
	require.Nil(t, statuses[0].ShareID)
	require.Equal(t, DeviceStatusData{
		Type:          GpuDeviceType,
		UUID:          "GPU-0000",
		ProductName:   "NVIDIA H100 80GB HBM3",
		DriverVersion: "565.57.01",
		PCIBusID:      "0000:01:00.0",
	}, decodeStatusData(t, statuses[0]))

	require.Equal(t, "gpu-1-mig-1g5gb-19-0", statuses[1].Device)
	require.Equal(t, DeviceStatusData{
		Type:          PreparedMigDeviceType,
		UUID:          "MIG-2222",
		ParentUUID:    "GPU-1111",
		ProductName:   "NVIDIA A100-SXM4-40GB",
		DriverVersion: "565.57.01",
		PCIBusID:      "0000:02:00.0",
		MigProfile:    "1g.5gb",
	}, decodeStatusData(t, statuses[1]))

	require.Equal(t, "gpu-vfio-0", statuses[2].Device)
	require.Equal(t, DeviceStatusData{
		Type:        VfioDeviceType,
		UUID:        "GPU-3333",
		ProductName: "NVIDIA H100 80GB HBM3",
		PCIBusID:    "0000:03:00.0",
	}, decodeStatusData(t, statuses[2]))
}

// A GPU prepared before a plugin restart is restored from the checkpoint,
// which round-trips only the UUID of the GpuInfo. The status data must still
// be complete, resolved from current device discovery data.
func TestBuildDeviceStatusesFromRestoredCheckpoint(t *testing.T) {
	state := deviceStatusTestState()

	claim := claimWithResults(resourceapi.DeviceRequestAllocationResult{
		Request: "req-gpu", Driver: DriverName, Pool: "node-1", Device: "gpu-0",
	})

	preparedDevices := PreparedDevices{
		&PreparedDeviceGroup{
			Devices: PreparedDeviceList{
				{
					Gpu: &PreparedGpu{
						// As deserialized from the checkpoint: unexported
						// GpuInfo fields (productName, ...) are all empty.
						Info:   &GpuInfo{UUID: "GPU-0000"},
						Device: &CheckpointedDevice{DeviceName: "gpu-0", PoolName: "node-1"},
					},
				},
			},
		},
	}

	statuses := state.buildDeviceStatuses(claim, preparedDevices)
	require.Len(t, statuses, 1)
	data := decodeStatusData(t, statuses[0])
	require.Equal(t, "NVIDIA H100 80GB HBM3", data.ProductName)
	require.Equal(t, "565.57.01", data.DriverVersion)
}

func TestBuildDeviceStatusesConsumableShares(t *testing.T) {
	state := deviceStatusTestState()

	shareA := types.UID("11111111-1111-1111-1111-111111111111")
	shareB := types.UID("22222222-2222-2222-2222-222222222222")
	claim := claimWithResults(
		resourceapi.DeviceRequestAllocationResult{
			Request: "req-a", Driver: DriverName, Pool: "node-1", Device: "gpu-0", ShareID: &shareA,
		},
		resourceapi.DeviceRequestAllocationResult{
			Request: "req-b", Driver: DriverName, Pool: "node-1", Device: "gpu-0", ShareID: &shareB,
		},
	)

	preparedDevices := PreparedDevices{
		&PreparedDeviceGroup{
			Devices: PreparedDeviceList{
				{
					Gpu: &PreparedGpu{
						Info:   &GpuInfo{UUID: "GPU-0000"},
						Device: &CheckpointedDevice{DeviceName: "gpu-0", PoolName: "node-1"},
					},
				},
			},
		},
	}

	statuses := state.buildDeviceStatuses(claim, preparedDevices)
	require.Len(t, statuses, 2)
	require.Equal(t, ptr.To(string(shareA)), statuses[0].ShareID)
	require.Equal(t, ptr.To(string(shareB)), statuses[1].ShareID)
}

// Two allocation results for the same device without a ShareID (e.g. from
// adminAccess requests) must not produce duplicate (driver, pool, device,
// shareID) keys: status.devices is validated as a set and the whole update
// would be rejected.
func TestBuildDeviceStatusesDeduplicatesResults(t *testing.T) {
	state := deviceStatusTestState()

	claim := claimWithResults(
		resourceapi.DeviceRequestAllocationResult{
			Request: "req-a", Driver: DriverName, Pool: "node-1", Device: "gpu-0",
		},
		resourceapi.DeviceRequestAllocationResult{
			Request: "req-b", Driver: DriverName, Pool: "node-1", Device: "gpu-0",
		},
	)

	preparedDevices := PreparedDevices{
		&PreparedDeviceGroup{
			Devices: PreparedDeviceList{
				{
					Gpu: &PreparedGpu{
						Info:   &GpuInfo{UUID: "GPU-0000"},
						Device: &CheckpointedDevice{DeviceName: "gpu-0", PoolName: "node-1"},
					},
				},
			},
		},
	}

	statuses := state.buildDeviceStatuses(claim, preparedDevices)
	require.Len(t, statuses, 1)
}

func TestBuildDeviceStatusesNoAllocation(t *testing.T) {
	state := deviceStatusTestState()
	claim := claimWithResults()
	claim.Status.Allocation = nil
	require.Empty(t, state.buildDeviceStatuses(claim, PreparedDevices{}))
}

func TestMergeDeviceStatuses(t *testing.T) {
	otherDriverStatus := resourceapi.AllocatedDeviceStatus{
		Driver: "other.example.com", Pool: "pool-x", Device: "dev-x",
	}
	staleOwnStatus := resourceapi.AllocatedDeviceStatus{
		Driver: DriverName, Pool: "node-1", Device: "gpu-9",
	}
	ownStatus := resourceapi.AllocatedDeviceStatus{
		Driver: DriverName, Pool: "node-1", Device: "gpu-0",
	}

	// Entries of other drivers are preserved; all entries owned by this
	// driver are replaced with the new set.
	merged := mergeDeviceStatuses(
		[]resourceapi.AllocatedDeviceStatus{otherDriverStatus, staleOwnStatus},
		[]resourceapi.AllocatedDeviceStatus{ownStatus},
	)
	require.Equal(t, []resourceapi.AllocatedDeviceStatus{otherDriverStatus, ownStatus}, merged)

	// No prior entries.
	merged = mergeDeviceStatuses(nil, []resourceapi.AllocatedDeviceStatus{ownStatus})
	require.Equal(t, []resourceapi.AllocatedDeviceStatus{ownStatus}, merged)

	// A nil replacement set (the Unprepare path) removes all of this
	// driver's entries and keeps everyone else's.
	merged = mergeDeviceStatuses([]resourceapi.AllocatedDeviceStatus{otherDriverStatus, ownStatus}, nil)
	require.Equal(t, []resourceapi.AllocatedDeviceStatus{otherDriverStatus}, merged)
}
