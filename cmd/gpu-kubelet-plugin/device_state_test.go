/*
Copyright The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	resourceapi "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"

	configapi "sigs.k8s.io/dra-driver-nvidia-gpu/api/nvidia.com/resource/v1beta1"
	"sigs.k8s.io/dra-driver-nvidia-gpu/pkg/fabricmanager"
	"sigs.k8s.io/dra-driver-nvidia-gpu/pkg/featuregates"
)

func TestValidateNoOverlappingPreparedDevices(t *testing.T) {
	perGPU := &PerGPUAllocatableDevices{
		allocatablesMap: map[PCIBusID]AllocatableDevices{
			"0000:00:00.0": {
				"gpu-0":  &AllocatableDevice{Gpu: &GpuInfo{minor: 0}},
				"vfio-0": &AllocatableDevice{Vfio: &VfioDeviceInfo{index: 0}},
			},
		},
	}

	checkpoint := &Checkpoint{
		V2: &CheckpointV2{
			PreparedClaims: PreparedClaimsByUID{
				"claim-1": {
					CheckpointState: ClaimCheckpointStatePrepareCompleted,
					Status: resourceapi.ResourceClaimStatus{
						Allocation: &resourceapi.AllocationResult{
							Devices: resourceapi.DeviceAllocationResult{
								Results: []resourceapi.DeviceRequestAllocationResult{
									{Driver: DriverName, Device: "gpu-0"},
									{Driver: DriverName, Device: "vfio-0"},
								},
							},
						},
					},
				},
			},
		},
	}

	tests := []struct {
		name                 string
		featureGate          bool
		consumableSharesFlag string
		requestDevice        string
		expectErr            bool
	}{
		{
			name:                 "gpu overlap rejected when consumable shares disabled",
			featureGate:          false,
			consumableSharesFlag: "disabled",
			requestDevice:        "gpu-0",
			expectErr:            true,
		},
		{
			name:                 "gpu overlap allowed when consumable shares enabled and matching configs",
			featureGate:          true,
			consumableSharesFlag: "unlimited",
			requestDevice:        "gpu-0",
			expectErr:            false,
		},
		{
			name:                 "vfio overlap rejected even when consumable shares enabled",
			featureGate:          true,
			consumableSharesFlag: "unlimited",
			requestDevice:        "vfio-0",
			expectErr:            true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			require.NoError(t, featuregates.FeatureGates().SetFromMap(map[string]bool{
				string(featuregates.ConsumableShares): tc.featureGate,
			}))

			state := &DeviceState{
				config: &Config{
					flags: &Flags{
						consumableShares: tc.consumableSharesFlag,
					},
				},
				perGPUAllocatable: perGPU,
			}

			incomingClaim := &resourceapi.ResourceClaim{
				Status: resourceapi.ResourceClaimStatus{
					Allocation: &resourceapi.AllocationResult{
						Devices: resourceapi.DeviceAllocationResult{
							Results: []resourceapi.DeviceRequestAllocationResult{
								{Driver: DriverName, Device: tc.requestDevice},
							},
						},
					},
				},
			}

			err := state.validateNoOverlappingPreparedDevices(checkpoint, incomingClaim)
			if tc.expectErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestGetAllocatableDevicesForClaim(t *testing.T) {
	gpu0 := &AllocatableDevice{Gpu: &GpuInfo{UUID: "GPU-0000"}}
	vfio1 := &AllocatableDevice{Vfio: &VfioDeviceInfo{UUID: "VFIO-0001"}}
	mig2 := &AllocatableDevice{MigDynamic: &MigSpec{Parent: &GpuInfo{UUID: "GPU-0002"}}}
	claimStatus := func(results ...resourceapi.DeviceRequestAllocationResult) resourceapi.ResourceClaimStatus {
		return resourceapi.ResourceClaimStatus{
			Allocation: &resourceapi.AllocationResult{
				Devices: resourceapi.DeviceAllocationResult{Results: results},
			},
		}
	}
	state := &DeviceState{
		perGPUAllocatable: &PerGPUAllocatableDevices{
			allocatablesMap: map[PCIBusID]AllocatableDevices{
				"0000:00:00.0": {
					"gpu-0": gpu0,
				},
				"0000:00:01.0": {
					"vfio-1": vfio1,
				},
				"0000:00:02.0": {
					"mig-2": mig2,
				},
			},
		},
	}

	tests := []struct {
		name   string
		status resourceapi.ResourceClaimStatus
		want   AllocatableDevices
	}{
		{
			name:   "nil allocation",
			status: resourceapi.ResourceClaimStatus{},
			want:   AllocatableDevices{},
		},
		{
			name:   "empty allocation results",
			status: claimStatus(),
			want:   AllocatableDevices{},
		},
		{
			name: "returns GPU device",
			status: claimStatus(
				resourceapi.DeviceRequestAllocationResult{Driver: DriverName, Device: "gpu-0"},
			),
			want: AllocatableDevices{
				"gpu-0": gpu0,
			},
		},
		{
			name: "returns VFIO device",
			status: claimStatus(
				resourceapi.DeviceRequestAllocationResult{Driver: DriverName, Device: "vfio-1"},
			),
			want: AllocatableDevices{
				"vfio-1": vfio1,
			},
		},
		{
			name: "returns MIG device",
			status: claimStatus(
				resourceapi.DeviceRequestAllocationResult{Driver: DriverName, Device: "mig-2"},
			),
			want: AllocatableDevices{
				"mig-2": mig2,
			},
		},
		{
			name: "ignores devices from other drivers",
			status: claimStatus(
				resourceapi.DeviceRequestAllocationResult{Driver: "other.example.com", Device: "gpu-0"},
			),
			want: AllocatableDevices{},
		},
		{
			name: "skips missing allocatable devices",
			status: claimStatus(
				resourceapi.DeviceRequestAllocationResult{Driver: DriverName, Device: "missing"},
				resourceapi.DeviceRequestAllocationResult{Driver: DriverName, Device: "gpu-0"},
			),
			want: AllocatableDevices{
				"gpu-0": gpu0,
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			pc := PreparedClaim{Status: tc.status}

			got := state.getAllocatableDevicesForClaim("claim-uid", pc)

			require.NotNil(t, got)
			require.Equal(t, tc.want, got)
		})
	}
}

func TestApplySharingConfigMpsDisallowedWithConsumableShares(t *testing.T) {
	perGPU := &PerGPUAllocatableDevices{
		allocatablesMap: map[PCIBusID]AllocatableDevices{
			"0000:00:00.0": {
				"gpu-0": &AllocatableDevice{Gpu: &GpuInfo{UUID: "GPU-0000", cudaComputeCapability: "8.0"}},
				"gpu-1": &AllocatableDevice{Gpu: &GpuInfo{UUID: "GPU-0001", cudaComputeCapability: "8.0"}},
			},
		},
	}

	config := &configapi.GpuSharing{
		Strategy:  configapi.MpsStrategy,
		MpsConfig: &configapi.MpsConfig{},
	}

	claim := &resourceapi.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{UID: "test-claim-uid"},
	}

	results := []*resourceapi.DeviceRequestAllocationResult{
		{Request: "req-0", Device: "gpu-0"},
		{Request: "req-1", Device: "gpu-1"},
	}

	t.Run("disallowed when consumable shares enabled", func(t *testing.T) {
		require.NoError(t, featuregates.FeatureGates().SetFromMap(map[string]bool{
			string(featuregates.MPSSupport):       true,
			string(featuregates.ConsumableShares): true,
		}))

		state := &DeviceState{
			config: &Config{
				flags: &Flags{
					consumableShares: "unlimited",
				},
			},
			perGPUAllocatable: perGPU,
		}

		_, err := state.applySharingConfig(context.Background(), config, claim, results, nil)
		require.Error(t, err)
		require.Contains(t, err.Error(), "MPS sharing is not supported when consumable shares is enabled")
	})

	t.Run("allowed when consumable shares disabled", func(t *testing.T) {
		require.NoError(t, featuregates.FeatureGates().SetFromMap(map[string]bool{
			string(featuregates.MPSSupport):       true,
			string(featuregates.ConsumableShares): false,
		}))

		cfg := &Config{
			flags: &Flags{
				nodeName:         "node-a",
				namespace:        "default",
				consumableShares: "disabled",
			},
		}
		state := &DeviceState{
			config:            cfg,
			mpsManager:        NewMpsManager(cfg, nil, "/", "/templates/mps-control-daemon.tmpl.yaml"),
			perGPUAllocatable: perGPU,
		}

		// Verify that applySharingConfig passes the consumable shares check (and reaches MPS daemon start)
		defer func() {
			r := recover()
			// Panic happens on uninitialized clientset inside Start(), which proves it passed consumable shares check
			if r == nil {
				t.Log("applySharingConfig completed without panic")
			}
		}()

		_, err := state.applySharingConfig(context.Background(), config, claim, results, nil)
		if err != nil {
			require.NotContains(t, err.Error(), "MPS sharing is not supported when consumable shares is enabled")
		}
	})
}

func TestSharingReferenceCountingHelpers(t *testing.T) {
	checkpoint := &Checkpoint{
		V2: &CheckpointV2{
			PreparedClaims: PreparedClaimsByUID{
				"claim-1": {
					CheckpointState: ClaimCheckpointStatePrepareCompleted,
					Status: resourceapi.ResourceClaimStatus{
						Allocation: &resourceapi.AllocationResult{
							Devices: resourceapi.DeviceAllocationResult{
								Results: []resourceapi.DeviceRequestAllocationResult{
									{Driver: DriverName, Device: "gpu-0"},
									{Driver: DriverName, Device: "mig-0"},
								},
							},
						},
					},
					PreparedDevices: PreparedDevices{
						{
							Devices: PreparedDeviceList{
								{
									Gpu: &PreparedGpu{
										Info: &GpuInfo{UUID: "GPU-1111"},
										Device: &CheckpointedDevice{
											DeviceName: "gpu-0",
										},
									},
								},
								{
									Mig: &PreparedMigDevice{
										Concrete: &MigLiveTuple{MigUUID: "MIG-2222"},
										Device: &CheckpointedDevice{
											DeviceName: "mig-0",
										},
									},
								},
							},
						},
					},
				},
				"claim-admin": {
					CheckpointState: ClaimCheckpointStatePrepareCompleted,
					Status: resourceapi.ResourceClaimStatus{
						Allocation: &resourceapi.AllocationResult{
							Devices: resourceapi.DeviceAllocationResult{
								Results: []resourceapi.DeviceRequestAllocationResult{
									{Driver: DriverName, Device: "gpu-admin", AdminAccess: ptr.To(true)},
									{Driver: DriverName, Device: "gpu-non-admin"},
								},
							},
						},
					},
					PreparedDevices: PreparedDevices{
						{
							Devices: PreparedDeviceList{
								{
									Gpu: &PreparedGpu{
										Info: &GpuInfo{UUID: "GPU-ADMIN"},
										Device: &CheckpointedDevice{
											DeviceName: "gpu-admin",
										},
									},
								},
								{
									Gpu: &PreparedGpu{
										Info: &GpuInfo{UUID: "GPU-NON-ADMIN"},
										Device: &CheckpointedDevice{
											DeviceName: "gpu-non-admin",
										},
									},
								},
							},
						},
					},
				},
			},
		},
	}

	// Active claim claim-1 uses GPU-1111 and mig-0 (MIG-2222).
	// Releasing claim-2 (a different claim) should detect that GPU-1111 and MIG-2222 are in use.
	require.True(t, isGpuUUIDInUseByOtherClaims(checkpoint, "claim-2", "GPU-1111"))
	require.False(t, isGpuUUIDInUseByOtherClaims(checkpoint, "claim-2", "GPU-9999"))

	// Releasing claim-1 should return false because claim-1 is being released.
	require.False(t, isGpuUUIDInUseByOtherClaims(checkpoint, "claim-1", "GPU-1111"))

	require.True(t, isMigDeviceInUseByOtherClaims(checkpoint, "claim-2", "MIG-2222", "mig-0"))
	require.False(t, isMigDeviceInUseByOtherClaims(checkpoint, "claim-2", "MIG-9999", "mig-9"))
	require.False(t, isMigDeviceInUseByOtherClaims(checkpoint, "claim-1", "MIG-2222", "mig-0"))

	require.False(t, isGpuUUIDInUseByOtherClaims(checkpoint, "claim-2", "GPU-ADMIN"))
	require.True(t, isGpuUUIDInUseByOtherClaims(checkpoint, "claim-2", "GPU-NON-ADMIN"))

	var cpNil *Checkpoint
	require.False(t, isGpuUUIDInUseByOtherClaims(cpNil, "claim-1", "GPU-1111"))
	require.False(t, isMigDeviceInUseByOtherClaims(cpNil, "claim-1", "MIG-2222", "mig-0"))
}

type testFMClient struct {
	partitions     []fabricmanager.Partition
	deactivatedIDs []int
}

func (c *testFMClient) Init() error     { return nil }
func (c *testFMClient) Shutdown() error { return nil }
func (c *testFMClient) GetSupportedFabricPartitions() ([]fabricmanager.Partition, error) {
	return c.partitions, nil
}
func (c *testFMClient) ActivateFabricPartition(id int) error { return nil }
func (c *testFMClient) DeactivateFabricPartition(id int) error {
	c.deactivatedIDs = append(c.deactivatedIDs, id)
	return nil
}
func (c *testFMClient) IsFabricPartitionActive(id int) (bool, error) {
	return false, nil
}

func TestDeactivateFabricPartitionRefCounting(t *testing.T) {
	require.NoError(t, featuregates.FeatureGates().SetFromMap(map[string]bool{
		string(featuregates.FabricManagerPartitioning): true,
		string(featuregates.ConsumableShares):          true,
	}))

	checkpoint := &Checkpoint{
		V2: &CheckpointV2{
			PreparedClaims: PreparedClaimsByUID{
				"claim-1": {
					CheckpointState: ClaimCheckpointStatePrepareCompleted,
					Status: resourceapi.ResourceClaimStatus{
						Allocation: &resourceapi.AllocationResult{
							Devices: resourceapi.DeviceAllocationResult{
								Results: []resourceapi.DeviceRequestAllocationResult{
									{Driver: DriverName, Device: "gpu-0"},
								},
							},
						},
					},
					PreparedDevices: PreparedDevices{
						{
							Devices: PreparedDeviceList{
								{
									Gpu: &PreparedGpu{
										Info: &GpuInfo{UUID: "GPU-0000", gpuModuleID: 1},
										Device: &CheckpointedDevice{
											DeviceName: "gpu-0",
										},
									},
								},
							},
						},
					},
				},
				"claim-2": {
					CheckpointState: ClaimCheckpointStatePrepareCompleted,
					Status: resourceapi.ResourceClaimStatus{
						Allocation: &resourceapi.AllocationResult{
							Devices: resourceapi.DeviceAllocationResult{
								Results: []resourceapi.DeviceRequestAllocationResult{
									{Driver: DriverName, Device: "gpu-0"},
								},
							},
						},
					},
					PreparedDevices: PreparedDevices{
						{
							Devices: PreparedDeviceList{
								{
									Gpu: &PreparedGpu{
										Info: &GpuInfo{UUID: "GPU-0000", gpuModuleID: 1},
										Device: &CheckpointedDevice{
											DeviceName: "gpu-0",
										},
									},
								},
							},
						},
					},
				},
			},
		},
	}

	fmClient := &testFMClient{
		partitions: []fabricmanager.Partition{
			{
				ID:       1,
				IsActive: true,
				GPUs: []fabricmanager.PartitionGPU{
					{PhysicalID: 1, UUID: "GPU-0000"},
				},
			},
		},
	}
	fmManager, err := fabricmanager.Open(fmClient)
	require.NoError(t, err)

	state := &DeviceState{
		config: &Config{
			flags: &Flags{
				consumableShares: "unlimited",
			},
		},
		fmManager: fmManager,
		perGPUAllocatable: &PerGPUAllocatableDevices{
			allocatablesMap: map[PCIBusID]AllocatableDevices{
				"0000:00:00.0": {
					"gpu-0": &AllocatableDevice{Gpu: &GpuInfo{UUID: "GPU-0000", gpuModuleID: 1}},
				},
			},
		},
	}

	pc1 := checkpoint.V2.PreparedClaims["claim-1"]

	// Case 1: Unpreparing claim-1 while claim-2 is still active on GPU-0000.
	// isGpuUUIDInUseByOtherClaims returns true -> deactivateFabricPartition returns nil early without calling FM DeactivatePartition.
	err = state.deactivateFabricPartition("claim-1", &pc1, checkpoint)
	require.NoError(t, err)
	require.Empty(t, fmClient.deactivatedIDs, "FM partition deactivation MUST NOT be called while claim-2 is active")

	// Case 2: Remove claim-2 from checkpoint so claim-1 is the sole claim.
	// Now deactivateFabricPartition calls FM DeactivatePartition(1).
	delete(checkpoint.V2.PreparedClaims, "claim-2")

	err = state.deactivateFabricPartition("claim-1", &pc1, checkpoint)
	require.NoError(t, err)
	require.Equal(t, []int{1}, fmClient.deactivatedIDs, "FM partition deactivation MUST be called when no active claims remain")
}

func TestValidateAdminAccessRequest(t *testing.T) {
	state := &DeviceState{
		perGPUAllocatable: &PerGPUAllocatableDevices{
			allocatablesMap: map[PCIBusID]AllocatableDevices{
				"0000:00:00.0": {
					"gpu-0":  &AllocatableDevice{Gpu: &GpuInfo{minor: 0}},
					"vfio-0": &AllocatableDevice{Vfio: &VfioDeviceInfo{index: 0}},
					"mig-0":  &AllocatableDevice{MigStatic: &MigDeviceInfo{}},
				},
			},
		},
	}

	// driverConfig builds a config for this driver applying to the given requests
	// (empty requests means it applies to every request in the claim).
	driverConfig := func(requests ...string) resourceapi.DeviceAllocationConfiguration {
		return resourceapi.DeviceAllocationConfiguration{
			Source:   resourceapi.AllocationConfigSourceClaim,
			Requests: requests,
			DeviceConfiguration: resourceapi.DeviceConfiguration{
				Opaque: &resourceapi.OpaqueDeviceConfiguration{Driver: DriverName},
			},
		}
	}
	otherDriverConfig := func(requests ...string) resourceapi.DeviceAllocationConfiguration {
		c := driverConfig(requests...)
		c.Opaque.Driver = "other.driver.com"
		return c
	}

	adminGpu := resourceapi.DeviceRequestAllocationResult{Driver: DriverName, Request: "gpu", Device: "gpu-0", AdminAccess: ptr.To(true)}
	nonAdminGpu := resourceapi.DeviceRequestAllocationResult{Driver: DriverName, Request: "gpu", Device: "gpu-0"}
	adminVfio := resourceapi.DeviceRequestAllocationResult{Driver: DriverName, Request: "gpu", Device: "vfio-0", AdminAccess: ptr.To(true)}
	adminMig := resourceapi.DeviceRequestAllocationResult{Driver: DriverName, Request: "gpu", Device: "mig-0", AdminAccess: ptr.To(true)}

	tests := []struct {
		name    string
		noAlloc bool
		results []resourceapi.DeviceRequestAllocationResult
		configs []resourceapi.DeviceAllocationConfiguration
		wantErr bool
	}{
		{
			name:    "no allocation",
			noAlloc: true,
		},
		{
			name:    "vfio without admin access",
			results: []resourceapi.DeviceRequestAllocationResult{{Driver: DriverName, Request: "gpu", Device: "vfio-0"}},
		},
		{
			name:    "vfio with admin access rejected",
			results: []resourceapi.DeviceRequestAllocationResult{adminVfio},
			wantErr: true,
		},
		{
			name:    "gpu with admin access, no config, allowed",
			results: []resourceapi.DeviceRequestAllocationResult{adminGpu},
		},
		{
			name:    "mig with admin access rejected",
			results: []resourceapi.DeviceRequestAllocationResult{adminMig},
			wantErr: true,
		},
		{
			name:    "mixed: gpu plus admin-access vfio rejected",
			results: []resourceapi.DeviceRequestAllocationResult{nonAdminGpu, adminVfio},
			wantErr: true,
		},
		{
			name:    "admin-access vfio for other driver ignored",
			results: []resourceapi.DeviceRequestAllocationResult{{Driver: "other.driver.com", Request: "gpu", Device: "vfio-0", AdminAccess: ptr.To(true)}},
		},
		{
			name:    "gpu admin access with config targeting the request rejected",
			results: []resourceapi.DeviceRequestAllocationResult{adminGpu},
			configs: []resourceapi.DeviceAllocationConfiguration{driverConfig("gpu")},
			wantErr: true,
		},
		{
			name:    "gpu admin access with claim-wide config rejected",
			results: []resourceapi.DeviceRequestAllocationResult{adminGpu},
			configs: []resourceapi.DeviceAllocationConfiguration{driverConfig()},
			wantErr: true,
		},
		{
			name:    "gpu admin access with config for another request allowed",
			results: []resourceapi.DeviceRequestAllocationResult{adminGpu},
			configs: []resourceapi.DeviceAllocationConfiguration{driverConfig("other")},
		},
		{
			name:    "non-admin gpu with config allowed",
			results: []resourceapi.DeviceRequestAllocationResult{nonAdminGpu},
			configs: []resourceapi.DeviceAllocationConfiguration{driverConfig("gpu")},
		},
		{
			name:    "gpu admin access with config for another driver allowed",
			results: []resourceapi.DeviceRequestAllocationResult{adminGpu},
			configs: []resourceapi.DeviceAllocationConfiguration{otherDriverConfig("gpu")},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			claim := &resourceapi.ResourceClaim{}
			if !tc.noAlloc {
				claim.Status.Allocation = &resourceapi.AllocationResult{
					Devices: resourceapi.DeviceAllocationResult{
						Results: tc.results,
						Config:  tc.configs,
					},
				}
			}
			err := state.validateAdminAccessRequest(claim)
			if tc.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestOwnedStatusKeepsOnlyOurResults(t *testing.T) {
	statusWith := func(results ...resourceapi.DeviceRequestAllocationResult) resourceapi.ResourceClaimStatus {
		return resourceapi.ResourceClaimStatus{Allocation: &resourceapi.AllocationResult{
			Devices: resourceapi.DeviceAllocationResult{Results: results},
		}}
	}

	t.Run("an unallocated claim is left alone", func(t *testing.T) {
		require.Nil(t, ownedStatus(resourceapi.ResourceClaimStatus{}).Allocation)
	})

	t.Run("results of other drivers are dropped", func(t *testing.T) {
		in := statusWith(
			resourceapi.DeviceRequestAllocationResult{Driver: "other.driver.com", Device: "foreign-0"},
			resourceapi.DeviceRequestAllocationResult{Driver: DriverName, Device: "gpu-0"},
		)

		got := ownedStatus(in)

		require.Equal(t,
			[]resourceapi.DeviceRequestAllocationResult{{Driver: DriverName, Device: "gpu-0"}},
			got.Allocation.Devices.Results)
	})

	// claim.Status comes from the informer cache, so filtering must not reach it.
	t.Run("the claim we were handed is not modified", func(t *testing.T) {
		in := statusWith(
			resourceapi.DeviceRequestAllocationResult{Driver: "other.driver.com", Device: "foreign-0"},
			resourceapi.DeviceRequestAllocationResult{Driver: DriverName, Device: "gpu-0"},
		)

		_ = ownedStatus(in)

		require.Len(t, in.Allocation.Devices.Results, 2)
	})

	// Every claim a single-driver node sees takes this path, so it must not pay
	// for a copy of the status.
	t.Run("a claim that is all ours is handed straight back", func(t *testing.T) {
		in := statusWith(resourceapi.DeviceRequestAllocationResult{Driver: DriverName, Device: "gpu-0"})

		got := ownedStatus(in)

		require.Same(t, in.Allocation, got.Allocation)
	})

	t.Run("an allocation with no results keeps having none", func(t *testing.T) {
		in := resourceapi.ResourceClaimStatus{Allocation: &resourceapi.AllocationResult{}}

		got := ownedStatus(in)

		require.Nil(t, got.Allocation.Devices.Results)
	})

	t.Run("a claim with nothing of ours ends up empty", func(t *testing.T) {
		in := statusWith(resourceapi.DeviceRequestAllocationResult{Driver: "other.driver.com", Device: "foreign-0"})

		got := ownedStatus(in)

		require.Empty(t, got.Allocation.Devices.Results)
	})
}

// An upgrade converts the checkpoint on read, which is a write of our own.
func TestCheckpointV1ToV2KeepsOnlyOurResults(t *testing.T) {
	v1 := &CheckpointV1{PreparedClaims: PreparedClaimsByUIDV1{
		"claim-uid": {Status: resourceapi.ResourceClaimStatus{
			Allocation: &resourceapi.AllocationResult{
				Devices: resourceapi.DeviceAllocationResult{Results: []resourceapi.DeviceRequestAllocationResult{
					{Driver: "other.driver.com", Device: "foreign-0"},
					{Driver: DriverName, Device: "gpu-0"},
				}},
			},
		}},
	}}

	v2 := v1.ToV2()

	require.Equal(t,
		[]resourceapi.DeviceRequestAllocationResult{{Driver: DriverName, Device: "gpu-0"}},
		v2.PreparedClaims["claim-uid"].Status.Allocation.Devices.Results)
}

func TestIsAdminAccessIgnoresOtherDrivers(t *testing.T) {
	results := []resourceapi.DeviceRequestAllocationResult{
		{Driver: "other.driver.com", AdminAccess: ptr.To(true)},
		{Driver: DriverName},
	}

	require.False(t, isAdminAccess(results))

	results = append(results, resourceapi.DeviceRequestAllocationResult{
		Driver:      DriverName,
		AdminAccess: ptr.To(true),
	})

	require.True(t, isAdminAccess(results))
}

func TestRequestedNonAdminDevices(t *testing.T) {
	state := &DeviceState{}

	claim := &resourceapi.ResourceClaim{
		Status: resourceapi.ResourceClaimStatus{
			Allocation: &resourceapi.AllocationResult{
				Devices: resourceapi.DeviceAllocationResult{
					Results: []resourceapi.DeviceRequestAllocationResult{
						{Driver: DriverName, Device: "gpu-0"},
						{Driver: DriverName, Device: "gpu-admin", AdminAccess: ptr.To(true)},
						{Driver: DriverName, Device: "gpu-explicit-nonadmin", AdminAccess: ptr.To(false)},
						{Driver: "other.driver.com", Device: "gpu-other"},
					},
				},
			},
		},
	}

	got := state.requestedNonAdminDevices(claim)

	require.Equal(t, map[string]struct{}{
		"gpu-0":                 {},
		"gpu-explicit-nonadmin": {},
	}, got)
}

func TestGetPreparedMigDevice(t *testing.T) {
	migDev := &PreparedMigDevice{
		Concrete: &MigLiveTuple{MigUUID: "MIG-2222"},
		Device:   &CheckpointedDevice{DeviceName: "mig-0"},
	}
	checkpoint := &Checkpoint{
		V2: &CheckpointV2{
			PreparedClaims: PreparedClaimsByUID{
				"claim-completed": {
					CheckpointState: ClaimCheckpointStatePrepareCompleted,
					PreparedDevices: PreparedDevices{
						{Devices: PreparedDeviceList{{Mig: migDev}}},
					},
				},
				"claim-pending": {
					CheckpointState: "Prepared", // not PrepareCompleted -> skipped
					PreparedDevices: PreparedDevices{
						{Devices: PreparedDeviceList{{Mig: &PreparedMigDevice{
							Concrete: &MigLiveTuple{MigUUID: "MIG-PENDING"},
							Device:   &CheckpointedDevice{DeviceName: "mig-pending"},
						}}}},
					},
				},
			},
		},
	}

	state := &DeviceState{}

	require.Same(t, migDev, state.getPreparedMigDevice(checkpoint, "mig-0"))
	require.Nil(t, state.getPreparedMigDevice(checkpoint, "mig-unknown"))
	// Device belongs to a non-completed claim, so it must not be returned.
	require.Nil(t, state.getPreparedMigDevice(checkpoint, "mig-pending"))
	require.Nil(t, state.getPreparedMigDevice(nil, "mig-0"))
	require.Nil(t, state.getPreparedMigDevice(&Checkpoint{}, "mig-0"))
}

func TestPreparedClaimDeviceHasAdminAccess(t *testing.T) {
	claim := &PreparedClaim{
		Status: resourceapi.ResourceClaimStatus{
			Allocation: &resourceapi.AllocationResult{
				Devices: resourceapi.DeviceAllocationResult{
					Results: []resourceapi.DeviceRequestAllocationResult{
						{Driver: DriverName, Device: "gpu-admin", AdminAccess: ptr.To(true)},
						{Driver: DriverName, Device: "gpu-plain"},
						{Driver: "other.driver.com", Device: "gpu-other", AdminAccess: ptr.To(true)},
					},
				},
			},
		},
	}

	require.True(t, preparedClaimDeviceHasAdminAccess(claim, "gpu-admin"))
	require.False(t, preparedClaimDeviceHasAdminAccess(claim, "gpu-plain"))
	// Admin access on another driver's result must not count for this driver.
	require.False(t, preparedClaimDeviceHasAdminAccess(claim, "gpu-other"))
	require.False(t, preparedClaimDeviceHasAdminAccess(claim, "gpu-missing"))
	require.False(t, preparedClaimDeviceHasAdminAccess(nil, "gpu-admin"))
	require.False(t, preparedClaimDeviceHasAdminAccess(&PreparedClaim{}, "gpu-admin"))
}

func TestGpuInfosFromPreparedClaim(t *testing.T) {
	gpuInfo := &GpuInfo{UUID: "GPU-A"}
	vfioParent := &GpuInfo{UUID: "GPU-B"}

	state := &DeviceState{
		perGPUAllocatable: &PerGPUAllocatableDevices{
			allocatablesMap: map[PCIBusID]AllocatableDevices{
				"0000:00:00.0": {
					"gpu-0":  &AllocatableDevice{Gpu: gpuInfo},
					"vfio-0": &AllocatableDevice{Vfio: &VfioDeviceInfo{parent: vfioParent}},
					"mig-0":  &AllocatableDevice{MigStatic: &MigDeviceInfo{}},
				},
			},
		},
	}

	results := []resourceapi.DeviceRequestAllocationResult{
		{Driver: DriverName, Device: "gpu-0"},
		{Driver: DriverName, Device: "vfio-0"},
		{Driver: DriverName, Device: "mig-0"},         // MIG -> unsupported for fabric partition, skipped
		{Driver: DriverName, Device: "ghost"},         // not allocatable, skipped
		{Driver: "other.driver.com", Device: "gpu-0"}, // other driver, skipped
	}

	got := state.gpuInfosFromPreparedClaim(results)

	require.Equal(t, []*GpuInfo{gpuInfo, vfioParent}, got)
}

func TestMatchesDeviceType(t *testing.T) {
	gpu := &AllocatableDevice{Gpu: &GpuInfo{}}
	mig := &AllocatableDevice{MigStatic: &MigDeviceInfo{}}
	vfio := &AllocatableDevice{Vfio: &VfioDeviceInfo{}}

	require.True(t, matchesDeviceType(&configapi.GpuConfig{}, gpu))
	require.False(t, matchesDeviceType(&configapi.GpuConfig{}, mig))

	require.True(t, matchesDeviceType(&configapi.MigDeviceConfig{}, mig))
	require.False(t, matchesDeviceType(&configapi.MigDeviceConfig{}, gpu))

	require.True(t, matchesDeviceType(&configapi.VfioDeviceConfig{}, vfio))
	require.False(t, matchesDeviceType(&configapi.VfioDeviceConfig{}, gpu))

	// Unrecognized config type never matches.
	require.False(t, matchesDeviceType(&resourceapi.ResourceClaim{}, gpu))
}

func TestValidateDeviceConfigType(t *testing.T) {
	gpu := &AllocatableDevice{Gpu: &GpuInfo{}}
	mig := &AllocatableDevice{MigStatic: &MigDeviceInfo{}}
	vfio := &AllocatableDevice{Vfio: &VfioDeviceInfo{}}
	result := &resourceapi.DeviceRequestAllocationResult{}

	tests := []struct {
		name    string
		config  runtime.Object
		dev     *AllocatableDevice
		wantErr bool
	}{
		{name: "gpu config on gpu", config: &configapi.GpuConfig{}, dev: gpu},
		{name: "gpu config on mig", config: &configapi.GpuConfig{}, dev: mig, wantErr: true},
		{name: "mig config on mig", config: &configapi.MigDeviceConfig{}, dev: mig},
		{name: "mig config on gpu", config: &configapi.MigDeviceConfig{}, dev: gpu, wantErr: true},
		{name: "vfio config on vfio", config: &configapi.VfioDeviceConfig{}, dev: vfio},
		{name: "vfio config on gpu", config: &configapi.VfioDeviceConfig{}, dev: gpu, wantErr: true},
		// Unknown config types are not type-checked here.
		{name: "unknown config type", config: &resourceapi.ResourceClaim{}, dev: gpu},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateDeviceConfigType(tc.config, tc.dev, result)
			if tc.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestNormalizeAndValidateConfig(t *testing.T) {
	cfg, err := normalizeAndValidateConfig(configapi.DefaultGpuConfig())
	require.NoError(t, err)
	require.NotNil(t, cfg)

	_, err = normalizeAndValidateConfig(&resourceapi.ResourceClaim{})
	require.Error(t, err)
}
