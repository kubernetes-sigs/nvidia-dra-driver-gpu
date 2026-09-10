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
	"encoding/json"
	"strconv"
	"sync"
	"testing"
	"time"

	resourceapi "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/cache"
	"k8s.io/dynamic-resource-allocation/kubeletplugin"
	"k8s.io/kubernetes/pkg/kubelet/checkpointmanager"
	"k8s.io/utils/ptr"
	cdiapi "tags.cncf.io/container-device-interface/pkg/cdi"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	configapi "sigs.k8s.io/dra-driver-nvidia-gpu/api/nvidia.com/resource/v1beta1"
	"sigs.k8s.io/dra-driver-nvidia-gpu/pkg/imex"
	nvfake "sigs.k8s.io/dra-driver-nvidia-gpu/pkg/nvidia.com/clientset/versioned/fake"
	nvinformers "sigs.k8s.io/dra-driver-nvidia-gpu/pkg/nvidia.com/informers/externalversions"
)

type fakeCheckpointManager struct {
	checkpoint *Checkpoint
	list       []string
}

func (m *fakeCheckpointManager) CreateCheckpoint(_ string, checkpoint checkpointmanager.Checkpoint) error {
	cp, ok := checkpoint.(*Checkpoint)
	if ok {
		m.checkpoint = cp.ToLatestVersion()
	}
	return nil
}

func (m *fakeCheckpointManager) GetCheckpoint(_ string, checkpoint checkpointmanager.Checkpoint) error {
	cp, ok := checkpoint.(*Checkpoint)
	if !ok {
		return nil
	}
	if m.checkpoint == nil {
		*cp = Checkpoint{}
		return nil
	}
	*cp = *m.checkpoint
	return nil
}

func (m *fakeCheckpointManager) RemoveCheckpoint(_ string) error {
	m.checkpoint = nil
	return nil
}

func (m *fakeCheckpointManager) ListCheckpoints() ([]string, error) {
	return m.list, nil
}

func TestGetOpaqueDeviceConfigs(t *testing.T) {
	t.Run("returns matching driver configs in class then claim order", func(t *testing.T) {
		configs := []resourceapi.DeviceAllocationConfiguration{
			opaqueConfig(t, resourceapi.AllocationConfigSourceClass, DriverName, []string{"class-request"}, channelConfig("class-domain")),
			opaqueConfig(t, resourceapi.AllocationConfigSourceClaim, "other.example.com", []string{"ignored"}, channelConfig("ignored-domain")),
			opaqueConfig(t, resourceapi.AllocationConfigSourceClaim, DriverName, []string{"claim-request"}, daemonConfig("claim-domain")),
		}

		got, err := GetOpaqueDeviceConfigs(configapi.StrictDecoder, DriverName, configs)

		require.NoError(t, err)
		require.Len(t, got, 2)

		assert.Equal(t, []string{"class-request"}, got[0].Requests)
		channel, ok := got[0].Config.(*configapi.ComputeDomainChannelConfig)
		require.True(t, ok)
		assert.Equal(t, "class-domain", channel.DomainID)

		assert.Equal(t, []string{"claim-request"}, got[1].Requests)
		daemon, ok := got[1].Config.(*configapi.ComputeDomainDaemonConfig)
		require.True(t, ok)
		assert.Equal(t, "claim-domain", daemon.DomainID)
	})

	t.Run("rejects invalid source", func(t *testing.T) {
		configs := []resourceapi.DeviceAllocationConfiguration{
			opaqueConfig(t, resourceapi.AllocationConfigSource("invalid"), DriverName, nil, channelConfig("domain")),
		}

		_, err := GetOpaqueDeviceConfigs(configapi.StrictDecoder, DriverName, configs)

		require.Error(t, err)
		assert.Contains(t, err.Error(), "invalid config source")
	})

	t.Run("rejects missing opaque config", func(t *testing.T) {
		configs := []resourceapi.DeviceAllocationConfiguration{
			{Source: resourceapi.AllocationConfigSourceClass},
		}

		_, err := GetOpaqueDeviceConfigs(configapi.StrictDecoder, DriverName, configs)

		require.Error(t, err)
		assert.Contains(t, err.Error(), "only opaque parameters are supported")
	})

	t.Run("wraps strict decode errors as permanent", func(t *testing.T) {
		configs := []resourceapi.DeviceAllocationConfiguration{
			{
				Source: resourceapi.AllocationConfigSourceClass,
				DeviceConfiguration: resourceapi.DeviceConfiguration{
					Opaque: &resourceapi.OpaqueDeviceConfiguration{
						Driver: DriverName,
						Parameters: runtime.RawExtension{
							Raw: []byte(`{"apiVersion":"resource.nvidia.com/v1beta1","kind":"ComputeDomainChannelConfig","domainID":"domain","unknown":"field"}`),
						},
					},
				},
			},
		}

		_, err := GetOpaqueDeviceConfigs(configapi.StrictDecoder, DriverName, configs)

		require.Error(t, err)
		assert.True(t, isPermanentError(err))
		assert.Contains(t, err.Error(), "error decoding config parameters")
	})
}

func TestGetConfigResultsMap(t *testing.T) {
	t.Run("uses default configs by device type", func(t *testing.T) {
		state := testDeviceState()
		status := claimStatus(
			[]resourceapi.DeviceRequestAllocationResult{
				allocationResult("channel-request", DriverName, "channel-0", nil),
				allocationResult("daemon-request", DriverName, "daemon-0", nil),
				allocationResult("other-request", "other.example.com", "channel-0", nil),
			},
		)

		got, err := state.getConfigResultsMap(&status, configapi.StrictDecoder)

		require.NoError(t, err)
		require.Len(t, got, 2)
		assertConfigResultDevices(t, got, ComputeDomainChannelType, "", []string{"channel-0"})
		assertConfigResultDevices(t, got, ComputeDomainDaemonType, "", []string{"daemon-0"})
	})

	t.Run("request configs override defaults and class configs", func(t *testing.T) {
		state := testDeviceState()
		status := claimStatus(
			[]resourceapi.DeviceRequestAllocationResult{
				allocationResult("channel-request", DriverName, "channel-0", nil),
				allocationResult("daemon-request", DriverName, "daemon-0", nil),
			},
			opaqueConfig(t, resourceapi.AllocationConfigSourceClass, DriverName, []string{"channel-request"}, channelConfig("class-domain")),
			opaqueConfig(t, resourceapi.AllocationConfigSourceClaim, DriverName, []string{"channel-request"}, channelConfig("claim-domain")),
			opaqueConfig(t, resourceapi.AllocationConfigSourceClaim, DriverName, []string{"daemon-request"}, daemonConfig("daemon-domain")),
		)

		got, err := state.getConfigResultsMap(&status, configapi.StrictDecoder)

		require.NoError(t, err)
		require.Len(t, got, 2)
		assertConfigResultDevices(t, got, ComputeDomainChannelType, "claim-domain", []string{"channel-0"})
		assertConfigResultDevices(t, got, ComputeDomainDaemonType, "daemon-domain", []string{"daemon-0"})
		assertNoConfigResult(t, got, ComputeDomainChannelType, "class-domain")
	})

	t.Run("rejects requested devices that are not allocatable", func(t *testing.T) {
		state := testDeviceState()
		status := claimStatus(
			[]resourceapi.DeviceRequestAllocationResult{
				allocationResult("channel-request", DriverName, "missing-device", nil),
			},
		)

		_, err := state.getConfigResultsMap(&status, configapi.StrictDecoder)

		require.Error(t, err)
		assert.Contains(t, err.Error(), "requested device is not allocatable")
	})

	t.Run("rejects request config with mismatched device type", func(t *testing.T) {
		state := testDeviceState()
		status := claimStatus(
			[]resourceapi.DeviceRequestAllocationResult{
				allocationResult("channel-request", DriverName, "channel-0", nil),
			},
			opaqueConfig(t, resourceapi.AllocationConfigSourceClaim, DriverName, []string{"channel-request"}, daemonConfig("daemon-domain")),
		)

		_, err := state.getConfigResultsMap(&status, configapi.StrictDecoder)

		require.Error(t, err)
		assert.Contains(t, err.Error(), "cannot apply ComputeDomainDaemonConfig")
	})
}

// v0.5.0 checkpointed a claim before it validated the config, so an entry whose
// daemon DomainID this build rejects can already be on disk. Unprepare has to be
// able to clear it from either state without touching the unvalidated path.
func TestUnprepareClearsALegacyCheckpointWithARejectedDomainID(t *testing.T) {
	status := claimStatus(
		[]resourceapi.DeviceRequestAllocationResult{
			allocationResult("daemon-request", DriverName, "daemon-0", nil),
		},
		opaqueConfig(t, resourceapi.AllocationConfigSourceClaim, DriverName, []string{"daemon-request"}, daemonConfig("../x")),
	)

	cases := map[string]struct {
		state       ClaimCheckpointState
		wantCleared bool
	}{
		"left mid-prepare":  {state: ClaimCheckpointStatePrepareStarted},
		"already completed": {state: ClaimCheckpointStatePrepareCompleted, wantCleared: true},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			cache, err := cdiapi.NewCache(cdiapi.WithSpecDirs(t.TempDir()), cdiapi.WithAutoRefresh(false))
			require.NoError(t, err)

			state := testDeviceState()
			state.checkpointManager = &fakeCheckpointManager{checkpoint: checkpointWithClaims(map[string]PreparedClaim{
				"claim-uid": {
					CheckpointState: tc.state,
					Status:          status,
					Name:            "claim-uid",
					Namespace:       "default",
				},
			})}
			state.computeDomainManager = &ComputeDomainManager{configFilesRoot: t.TempDir()}
			state.cdi = &CDIHandler{cache: cache, vendor: cdiVendor, claimClass: cdiClaimClass}
			state.config = &Config{flags: &Flags{nodeName: "test-node"}}

			claimRef := kubeletplugin.NamespacedObject{
				NamespacedName: types.NamespacedName{Namespace: "default", Name: "claim-uid"},
				UID:            types.UID("claim-uid"),
			}
			require.NoError(t, state.Unprepare(context.Background(), claimRef))

			pc, stillThere := requireFakeCheckpointManager(t, state).checkpoint.V2.PreparedClaims["claim-uid"]
			if tc.wantCleared {
				require.False(t, stillThere, "a completed claim should be dropped from the checkpoint")
				return
			}
			require.True(t, stillThere)
			require.Equal(t, ClaimCheckpointStatePrepareAborted, pc.CheckpointState)
			require.NotNil(t, pc.AbortedAt)
		})
	}
}

// validateConfigs is the whole of the second half of the preflight, so a claim
// carrying one good and one bad config has to come back with nothing to apply,
// whichever order the map hands them over in.
func TestValidateConfigsReturnsNothingWhenAnyConfigIsInvalid(t *testing.T) {
	good := daemonConfig("d3b07384-d9a7-4e2b-8f1a-2c1e6b5a9f00")
	bad := channelConfig("../x")

	validated, err := validateConfigs(map[runtime.Object][]*resourceapi.DeviceRequestAllocationResult{
		good: nil,
		bad:  nil,
	})

	require.Error(t, err)
	require.True(t, isPermanentError(err), "an invalid config cannot become valid on a retry, got %v", err)
	require.Nil(t, validated, "returning the configs it did accept would let the caller apply them")
}

// A claim the plugin turns down on its own contents must not reach the
// checkpoint: Unprepare would have to decode that entry to clear it, which is
// the very thing that just failed.
func TestPrepareLeavesNoCheckpointForARejectedClaim(t *testing.T) {
	daemonRequest := allocationResult("daemon-request", DriverName, "daemon-0", nil)

	cases := map[string]resourceapi.DeviceAllocationConfiguration{
		"a kind this build does not know": rawOpaqueConfig([]string{"daemon-request"},
			`{"apiVersion":"resource.nvidia.com/v1beta1","kind":"FutureComputeDomainDaemonConfig","domainID":"d3b07384-d9a7-4e2b-8f1a-2c1e6b5a9f00"}`),
		"a field of the wrong type": rawOpaqueConfig([]string{"daemon-request"},
			`{"apiVersion":"resource.nvidia.com/v1beta1","kind":"ComputeDomainDaemonConfig","domainID":123}`),
		"a domainID that is not a path segment": rawOpaqueConfig([]string{"daemon-request"},
			`{"apiVersion":"resource.nvidia.com/v1beta1","kind":"ComputeDomainDaemonConfig","domainID":"../x"}`),
	}

	for name, config := range cases {
		t.Run(name, func(t *testing.T) {
			claim := &resourceapi.ResourceClaim{
				ObjectMeta: metav1.ObjectMeta{Name: "claim-uid", Namespace: "default", UID: types.UID("claim-uid")},
				Status:     claimStatus([]resourceapi.DeviceRequestAllocationResult{daemonRequest}, config),
			}
			state := testDeviceState()
			state.checkpointManager = &fakeCheckpointManager{checkpoint: checkpointWithClaims(map[string]PreparedClaim{})}
			state.computeDomainManager = &ComputeDomainManager{configFilesRoot: t.TempDir()}
			state.config = &Config{flags: &Flags{nodeName: "test-node"}}

			_, err := state.Prepare(context.Background(), claim)

			require.Error(t, err)
			require.True(t, isPermanentError(err), "a claim rejected on its contents cannot become valid on a retry, got %v", err)
			_, exists := requireFakeCheckpointManager(t, state).checkpoint.V2.PreparedClaims["claim-uid"]
			require.False(t, exists, "the claim was checkpointed even though nothing was applied for it")
		})
	}
}

// The preflight is only worth anything if Prepare still runs all of it before
// applying, so keep a case on the caller: with a good and a bad config the error
// has to be the deterministic one from validation rather than whatever applying
// the good one first would have produced.
func TestPrepareRejectsAMixedClaimBeforeApplying(t *testing.T) {
	state := testDeviceState()
	state.config = &Config{flags: &Flags{nodeName: "test-node"}}
	state.computeDomainManager = &ComputeDomainManager{configFilesRoot: t.TempDir()}
	claim := &resourceapi.ResourceClaim{
		Status: claimStatus(
			[]resourceapi.DeviceRequestAllocationResult{
				allocationResult("daemon-request", DriverName, "daemon-0", nil),
				allocationResult("channel-request", DriverName, "channel-0", nil),
			},
			opaqueConfig(t, resourceapi.AllocationConfigSourceClaim, DriverName, []string{"daemon-request"}, daemonConfig("d3b07384-d9a7-4e2b-8f1a-2c1e6b5a9f00")),
			opaqueConfig(t, resourceapi.AllocationConfigSourceClaim, DriverName, []string{"channel-request"}, channelConfig("../x")),
		),
	}

	// Map order decides which config a single-pass caller would reach first, so
	// repeat: the error has to come from validation every time, never from
	// applying the good one.
	for i := 0; i < 10; i++ {
		_, err := state.buildPreparePlan(claim)

		require.Error(t, err)
		require.True(t, isPermanentError(err), "applying before validating would surface an apply error instead, got %v", err)
		require.Contains(t, err.Error(), "error validating config")
	}
}

// rawOpaqueConfig carries JSON the plugin has to decode itself, which is how a
// hand-written claim reaches it.
func rawOpaqueConfig(requests []string, raw string) resourceapi.DeviceAllocationConfiguration {
	return resourceapi.DeviceAllocationConfiguration{
		Source:   resourceapi.AllocationConfigSourceClaim,
		Requests: requests,
		DeviceConfiguration: resourceapi.DeviceConfiguration{
			Opaque: &resourceapi.OpaqueDeviceConfiguration{
				Driver:     DriverName,
				Parameters: runtime.RawExtension{Raw: []byte(raw)},
			},
		},
	}
}

func TestRequestedNonAdminDevices(t *testing.T) {
	state := &DeviceState{}
	claim := claimWithResults(
		"claim-uid",
		allocationResult("request-1", DriverName, "channel-0", nil),
		allocationResult("request-2", DriverName, "daemon-0", ptr.To(false)),
		allocationResult("request-3", DriverName, "admin-device", ptr.To(true)),
		allocationResult("request-4", "other.example.com", "other-device", nil),
	)

	got := state.requestedNonAdminDevices(claim)

	assert.Equal(t, map[string]struct{}{
		"channel-0": {},
		"daemon-0":  {},
	}, got)
}

func TestValidateNoOverlappingPreparedDevices(t *testing.T) {
	tests := []struct {
		name       string
		checkpoint *Checkpoint
		claim      *resourceapi.ResourceClaim
		wantErr    string
	}{
		{
			name: "rejects non-admin overlap with completed different claim",
			checkpoint: checkpointWithClaims(map[string]PreparedClaim{
				"existing-uid": preparedClaim(ClaimCheckpointStatePrepareCompleted, allocationResult("request", DriverName, "channel-0", nil)),
			}),
			claim:   claimWithResults("incoming-uid", allocationResult("request", DriverName, "channel-0", nil)),
			wantErr: "requested device channel-0 is already allocated to different claim existing-uid",
		},
		{
			name: "allows overlap with same claim",
			checkpoint: checkpointWithClaims(map[string]PreparedClaim{
				"same-uid": preparedClaim(ClaimCheckpointStatePrepareCompleted, allocationResult("request", DriverName, "channel-0", nil)),
			}),
			claim: claimWithResults("same-uid", allocationResult("request", DriverName, "channel-0", nil)),
		},
		{
			name: "ignores incomplete prepared claims",
			checkpoint: checkpointWithClaims(map[string]PreparedClaim{
				"existing-uid": preparedClaim(ClaimCheckpointStatePrepareStarted, allocationResult("request", DriverName, "channel-0", nil)),
			}),
			claim: claimWithResults("incoming-uid", allocationResult("request", DriverName, "channel-0", nil)),
		},
		{
			name: "allows current admin access overlap",
			checkpoint: checkpointWithClaims(map[string]PreparedClaim{
				"existing-uid": preparedClaim(ClaimCheckpointStatePrepareCompleted, allocationResult("request", DriverName, "channel-0", nil)),
			}),
			claim: claimWithResults("incoming-uid", allocationResult("request", DriverName, "channel-0", ptr.To(true))),
		},
		{
			name: "allows existing admin access overlap",
			checkpoint: checkpointWithClaims(map[string]PreparedClaim{
				"existing-uid": preparedClaim(ClaimCheckpointStatePrepareCompleted, allocationResult("request", DriverName, "channel-0", ptr.To(true))),
			}),
			claim: claimWithResults("incoming-uid", allocationResult("request", DriverName, "channel-0", nil)),
		},
		{
			name: "ignores other drivers",
			checkpoint: checkpointWithClaims(map[string]PreparedClaim{
				"existing-uid": preparedClaim(ClaimCheckpointStatePrepareCompleted, allocationResult("request", "other.example.com", "channel-0", nil)),
			}),
			claim: claimWithResults("incoming-uid", allocationResult("request", DriverName, "channel-0", nil)),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			state := &DeviceState{}

			err := state.validateNoOverlappingPreparedDevices(tc.checkpoint, tc.claim)

			if tc.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

func TestAssertImexChannelNotAllocated(t *testing.T) {
	tests := []struct {
		name       string
		checkpoint *Checkpoint
		channelID  int
		wantErr    string
	}{
		{
			name: "rejects completed claim using channel",
			checkpoint: checkpointWithClaims(map[string]PreparedClaim{
				"existing-uid": {
					CheckpointState: ClaimCheckpointStatePrepareCompleted,
					PreparedDevices: PreparedDevices{
						{Devices: PreparedDeviceList{preparedChannel(0)}},
					},
				},
			}),
			channelID: 0,
			wantErr:   "channel 0 already allocated by claim existing-uid",
		},
		{
			name: "allows different channel",
			checkpoint: checkpointWithClaims(map[string]PreparedClaim{
				"existing-uid": {
					CheckpointState: ClaimCheckpointStatePrepareCompleted,
					PreparedDevices: PreparedDevices{
						{Devices: PreparedDeviceList{preparedChannel(1)}},
					},
				},
			}),
			channelID: 0,
		},
		{
			name: "ignores incomplete claim",
			checkpoint: checkpointWithClaims(map[string]PreparedClaim{
				"existing-uid": {
					CheckpointState: ClaimCheckpointStatePrepareStarted,
					PreparedDevices: PreparedDevices{
						{Devices: PreparedDeviceList{preparedChannel(0)}},
					},
				},
			}),
			channelID: 0,
		},
		{
			name: "ignores daemon devices",
			checkpoint: checkpointWithClaims(map[string]PreparedClaim{
				"existing-uid": {
					CheckpointState: ClaimCheckpointStatePrepareCompleted,
					PreparedDevices: PreparedDevices{
						{Devices: PreparedDeviceList{preparedDaemon(0)}},
					},
				},
			}),
			channelID: 0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			state := &DeviceState{
				checkpointManager: &fakeCheckpointManager{checkpoint: tc.checkpoint},
			}

			err := state.assertImexChannelNotAllocated(tc.channelID)

			if tc.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

func TestPrepareReturnsCheckpointedDevicesForCompletedClaim(t *testing.T) {
	expectedDevice := CheckpointedDevice{
		Requests:     []string{"request"},
		PoolName:     "pool",
		DeviceName:   "channel-0",
		CDIDeviceIDs: []string{"claim-device"},
	}
	checkpoint := checkpointWithClaims(map[string]PreparedClaim{
		"claim-uid": {
			CheckpointState: ClaimCheckpointStatePrepareCompleted,
			PreparedDevices: PreparedDevices{
				{
					Devices: PreparedDeviceList{
						{
							Channel: &PreparedComputeDomainChannel{
								Info:   &ComputeDomainChannelInfo{ID: 0},
								Device: &expectedDevice,
							},
						},
					},
				},
			},
		},
	})
	state := &DeviceState{
		checkpointManager: &fakeCheckpointManager{checkpoint: checkpoint},
	}
	claim := claimWithResults("claim-uid", allocationResult("request", DriverName, "channel-0", nil))

	got, err := state.Prepare(context.Background(), claim)

	require.NoError(t, err)
	assert.Equal(t, []kubeletplugin.Device{kubeletplugin.Device(expectedDevice)}, got)
}

func TestPrepareRejectsMatchingPrepareAbortedEntry(t *testing.T) {
	now := metav1.Now()
	claim := claimWithResults("claim-uid", allocationResult("request", DriverName, "channel-0", nil))
	checkpoint := checkpointWithClaims(map[string]PreparedClaim{
		"claim-uid": {
			CheckpointState: ClaimCheckpointStatePrepareAborted,
			Status:          claim.Status,
			Name:            claim.Name,
			Namespace:       claim.Namespace,
			AbortedAt:       &now,
		},
	})
	state := &DeviceState{
		checkpointManager: &fakeCheckpointManager{checkpoint: checkpoint},
	}

	got, err := state.Prepare(context.Background(), claim)

	require.Error(t, err)
	assert.True(t, isPermanentError(err))
	assert.Contains(t, err.Error(), "claim prepare was already aborted")
	assert.Nil(t, got)
	assert.Equal(t, ClaimCheckpointStatePrepareAborted, requireFakeCheckpointManager(t, state).checkpoint.V2.PreparedClaims["claim-uid"].CheckpointState)
}

func TestClaimMatchesPreparedClaim(t *testing.T) {
	claim := claimWithResults("claim-uid", allocationResult("request", DriverName, "channel-0", nil))

	assert.True(t, claimMatchesPreparedClaim(PreparedClaim{
		Status: claim.Status,
	}, claim))

	assert.False(t, claimMatchesPreparedClaim(PreparedClaim{
		Status: claimStatus([]resourceapi.DeviceRequestAllocationResult{
			allocationResult("request", DriverName, "daemon-0", nil),
		}),
	}, claim))
}

func TestMarkClaimPrepareAbortedInCheckpointWritesEntry(t *testing.T) {
	checkpoint := checkpointWithClaims(map[string]PreparedClaim{
		"claim-uid": {
			CheckpointState: ClaimCheckpointStatePrepareStarted,
			Status:          claimStatus([]resourceapi.DeviceRequestAllocationResult{allocationResult("request", DriverName, "channel-0", nil)}),
			PreparedDevices: PreparedDevices{
				{Devices: PreparedDeviceList{preparedChannel(0)}},
			},
		},
	})
	state := testCheckpointDeviceState(checkpoint)
	claimRef := kubeletplugin.NamespacedObject{
		NamespacedName: types.NamespacedName{
			Namespace: "default",
			Name:      "claim",
		},
		UID: "claim-uid",
	}
	pc := checkpoint.V2.PreparedClaims["claim-uid"]

	err := state.markClaimPrepareAbortedInCheckpoint(claimRef, pc)

	require.NoError(t, err)
	stored := requireFakeCheckpointManager(t, state).checkpoint.V2.PreparedClaims["claim-uid"]
	assert.Equal(t, ClaimCheckpointStatePrepareAborted, stored.CheckpointState)
	assert.Equal(t, "claim", stored.Name)
	assert.Equal(t, "default", stored.Namespace)
	assert.Nil(t, stored.PreparedDevices)
	require.NotNil(t, stored.AbortedAt)
}

func TestDeleteClaimFromCheckpoint(t *testing.T) {
	checkpoint := checkpointWithClaims(map[string]PreparedClaim{
		"claim-uid": {
			CheckpointState: ClaimCheckpointStatePrepareCompleted,
		},
	})
	state := testCheckpointDeviceState(checkpoint)
	claimRef := kubeletplugin.NamespacedObject{
		NamespacedName: types.NamespacedName{
			Namespace: "default",
			Name:      "claim",
		},
		UID: "claim-uid",
	}

	err := state.deleteClaimFromCheckpoint(claimRef)

	require.NoError(t, err)
	assert.NotContains(t, requireFakeCheckpointManager(t, state).checkpoint.V2.PreparedClaims, "claim-uid")
}

func TestDeleteExpiredPrepareAbortedClaimsFromCheckpoint(t *testing.T) {
	now := time.Now()
	old := metav1.NewTime(now.Add(-PrepareAbortedClaimEntryTTL - time.Second))
	fresh := metav1.NewTime(now.Add(-PrepareAbortedClaimEntryTTL + time.Second))
	checkpoint := checkpointWithClaims(map[string]PreparedClaim{
		"old": {
			CheckpointState: ClaimCheckpointStatePrepareAborted,
			AbortedAt:       &old,
		},
		"fresh": {
			CheckpointState: ClaimCheckpointStatePrepareAborted,
			AbortedAt:       &fresh,
		},
		"completed": {
			CheckpointState: ClaimCheckpointStatePrepareCompleted,
		},
	})
	state := testCheckpointDeviceState(checkpoint)

	deleted, err := state.deleteExpiredPrepareAbortedClaimsFromCheckpoint(now, PrepareAbortedClaimEntryTTL)

	require.NoError(t, err)
	assert.Equal(t, 1, deleted)
	stored := requireFakeCheckpointManager(t, state).checkpoint.V2.PreparedClaims
	assert.NotContains(t, stored, "old")
	assert.Contains(t, stored, "fresh")
	assert.Contains(t, stored, "completed")
}

func TestUnprepareMissingClaimIsNoop(t *testing.T) {
	state := &DeviceState{
		checkpointManager: &fakeCheckpointManager{checkpoint: checkpointWithClaims(nil)},
	}
	claimRef := kubeletplugin.NamespacedObject{
		NamespacedName: types.NamespacedName{
			Namespace: "default",
			Name:      "missing",
		},
		UID: "missing-uid",
	}

	err := state.Unprepare(context.Background(), claimRef)

	require.NoError(t, err)
}

func hostManagedConfig() *Config {
	return &Config{
		flags:      &Flags{imexHostSocketPath: defaultIMEXHostSocketPath},
		imexConfig: imex.Config{Mode: imex.ModeHostManaged, Isolation: imex.IsolationIMEXDomain},
	}
}

// TestApplyComputeDomainChannelConfigHostManagedIgnoresAllocationMode
// confirms the kubelet plugin does not (re-)validate AllocationMode under
// host-managed IMEX: today the controller is the one that always requests
// AllocationMode Single (channel 0) for host-managed ComputeDomains, so the
// kubelet plugin trusts it rather than duplicating that policy. This leaves
// room for the controller to later request other channels without needing a
// matching change here.
func TestApplyComputeDomainChannelConfigHostManagedIgnoresAllocationMode(t *testing.T) {
	for _, mode := range []string{"", configapi.ComputeDomainChannelAllocationModeSingle, configapi.ComputeDomainChannelAllocationModeAll, "foo"} {
		t.Run(mode, func(t *testing.T) {
			cd := &configapi.ComputeDomain{
				ObjectMeta: metav1.ObjectMeta{Name: "cd", Namespace: "default", UID: "cd-uid"},
			}

			// Empty cliqueID keeps this fixture minimal (no nvdevlib/cdi
			// setup needed): it only needs to reach past the removed
			// AllocationMode check to prove the mode is no longer rejected.
			factory := nvinformers.NewSharedInformerFactory(nvfake.NewSimpleClientset(), 0)
			informer := factory.Resource().V1beta1().ComputeDomains().Informer()
			require.NoError(t, informer.AddIndexers(cache.Indexers{"computeDomainUID": uidIndexer[*configapi.ComputeDomain]}))
			require.NoError(t, informer.GetIndexer().Add(cd))

			state := &DeviceState{
				config:               hostManagedConfig(),
				checkpointManager:    &fakeCheckpointManager{checkpoint: checkpointWithClaims(nil)},
				computeDomainManager: &ComputeDomainManager{informer: informer, cliqueID: ""},
				allocatable: AllocatableDevices{
					"channel-0": &AllocatableDevice{Channel: &ComputeDomainChannelInfo{ID: 0}},
				},
			}

			result := allocationResult("request", DriverName, "channel-0", nil)
			config := channelConfig("cd-uid")
			config.AllocationMode = mode

			_, err := state.applyComputeDomainChannelConfig(
				context.Background(),
				config,
				claimWithResults("claim-uid", result),
				[]*resourceapi.DeviceRequestAllocationResult{&result},
			)

			require.NoError(t, err, "AllocationMode %q must not be rejected by the kubelet plugin under host-managed IMEX", mode)
		})
	}
}

func TestApplyComputeDomainDaemonConfigHostManagedRejected(t *testing.T) {
	result := allocationResult("request", DriverName, "daemon-0", nil)

	state := &DeviceState{config: hostManagedConfig()}
	_, err := state.applyComputeDomainDaemonConfig(
		context.Background(),
		daemonConfig("cd-uid"),
		claimWithResults("claim-uid", result),
		[]*resourceapi.DeviceRequestAllocationResult{&result},
	)

	require.Error(t, err)
	assert.True(t, isPermanentError(err), "daemon claims must be a permanent error under host-managed IMEX")
}

func TestApplyComputeDomainChannelConfigHostManagedNoCliqueSkipsInjection(t *testing.T) {
	cd := &configapi.ComputeDomain{
		ObjectMeta: metav1.ObjectMeta{Name: "cd", Namespace: "default", UID: "cd-uid"},
	}

	// A ComputeDomainManager whose informer already holds the CD (so the
	// namespace assertion passes) but with an empty cliqueID, simulating a
	// non-MNNVL node with no NVLink fabric.
	factory := nvinformers.NewSharedInformerFactory(nvfake.NewSimpleClientset(), 0)
	informer := factory.Resource().V1beta1().ComputeDomains().Informer()
	require.NoError(t, informer.AddIndexers(cache.Indexers{"computeDomainUID": uidIndexer[*configapi.ComputeDomain]}))
	require.NoError(t, informer.GetIndexer().Add(cd))

	state := &DeviceState{
		config:               hostManagedConfig(),
		checkpointManager:    &fakeCheckpointManager{checkpoint: checkpointWithClaims(nil)},
		computeDomainManager: &ComputeDomainManager{informer: informer, cliqueID: ""},
		allocatable: AllocatableDevices{
			"channel-0": &AllocatableDevice{Channel: &ComputeDomainChannelInfo{ID: 0}},
		},
	}

	config := channelConfig("cd-uid")
	config.AllocationMode = configapi.ComputeDomainChannelAllocationModeSingle
	result := allocationResult("request", DriverName, "channel-0", nil)
	claim := claimWithResults("claim-uid", result)

	// applyComputeDomainChannelConfigHostManaged never calls
	// AddNodeLabel/AssertComputeDomainReady at all (unlike the driver-managed
	// path), which would otherwise dereference the manager's nil config;
	// reaching the clique check (without panicking) proves the host-managed
	// branch was taken.
	//
	// Like the driver-managed path, a missing clique must not fail the claim:
	// some nodes in a cluster are legitimately non-MNNVL, and claim
	// preparation there should still succeed, just without an injected
	// channel device node.
	configState, err := state.applyComputeDomainChannelConfig(
		context.Background(),
		config,
		claim,
		[]*resourceapi.DeviceRequestAllocationResult{&result},
	)

	require.NoError(t, err)
	require.NotNil(t, configState)
	assert.Equal(t, ComputeDomainChannelType, configState.Type)
	assert.Nil(t, configState.containerEdits, "no clique on this node means no IMEX channel device node is injected")
}

// TestApplyComputeDomainChannelConfigHostManagedUsesAllocatedChannel confirms
// that the channel-conflict check operates on whichever channel was actually
// allocated for the claim (via result.Device), not a hardcoded channel 0.
// Today the controller only ever requests channel 0, but this proves the
// kubelet plugin itself imposes no such restriction.
func TestApplyComputeDomainChannelConfigHostManagedUsesAllocatedChannel(t *testing.T) {
	cd := &configapi.ComputeDomain{
		ObjectMeta: metav1.ObjectMeta{Name: "cd", Namespace: "default", UID: "cd-uid"},
	}
	factory := nvinformers.NewSharedInformerFactory(nvfake.NewSimpleClientset(), 0)
	informer := factory.Resource().V1beta1().ComputeDomains().Informer()
	require.NoError(t, informer.AddIndexers(cache.Indexers{"computeDomainUID": uidIndexer[*configapi.ComputeDomain]}))
	require.NoError(t, informer.GetIndexer().Add(cd))

	// Seed the checkpoint with channel 2 (not channel 0) already allocated by
	// another claim.
	checkpoint := checkpointWithClaims(map[string]PreparedClaim{
		"existing-uid": {
			CheckpointState: ClaimCheckpointStatePrepareCompleted,
			PreparedDevices: PreparedDevices{
				{Devices: PreparedDeviceList{preparedChannel(2)}},
			},
		},
	})

	state := &DeviceState{
		config:               hostManagedConfig(),
		checkpointManager:    &fakeCheckpointManager{checkpoint: checkpoint},
		computeDomainManager: &ComputeDomainManager{informer: informer, cliqueID: ""},
		allocatable: AllocatableDevices{
			"channel-2": &AllocatableDevice{Channel: &ComputeDomainChannelInfo{ID: 2}},
		},
	}

	config := channelConfig("cd-uid")
	result := allocationResult("request", DriverName, "channel-2", nil)
	claim := claimWithResults("claim-uid", result)

	_, err := state.applyComputeDomainChannelConfig(
		context.Background(),
		config,
		claim,
		[]*resourceapi.DeviceRequestAllocationResult{&result},
	)

	require.Error(t, err, "channel 2 is already allocated by another claim, so this must fail")
	assert.Contains(t, err.Error(), "channel 2 already allocated")
}

// TestApplyComputeDomainChannelConfigHostManagedRequiresHostIMEXReady confirms
// that on a fabric node (non-empty cliqueID), a host-managed channel claim is
// rejected when the operator's host nvidia-imex daemon cannot be validated as
// ready (here: nvidia-imex-ctl isn't even present under the driver root).
// This is the only readiness signal host-managed mode has, since there is no
// driver-managed daemon to report ComputeDomain readiness.
func TestApplyComputeDomainChannelConfigHostManagedRequiresHostIMEXReady(t *testing.T) {
	cd := &configapi.ComputeDomain{
		ObjectMeta: metav1.ObjectMeta{Name: "cd", Namespace: "default", UID: "cd-uid"},
	}
	factory := nvinformers.NewSharedInformerFactory(nvfake.NewSimpleClientset(), 0)
	informer := factory.Resource().V1beta1().ComputeDomains().Informer()
	require.NoError(t, informer.AddIndexers(cache.Indexers{"computeDomainUID": uidIndexer[*configapi.ComputeDomain]}))
	require.NoError(t, informer.GetIndexer().Add(cd))

	state := &DeviceState{
		config:               hostManagedConfig(),
		checkpointManager:    &fakeCheckpointManager{checkpoint: checkpointWithClaims(nil)},
		computeDomainManager: &ComputeDomainManager{informer: informer, cliqueID: "clique-a"},
		nvdevlib:             &deviceLib{devRoot: t.TempDir()},
		allocatable: AllocatableDevices{
			"channel-0": &AllocatableDevice{Channel: &ComputeDomainChannelInfo{ID: 0}},
		},
	}

	config := channelConfig("cd-uid")
	result := allocationResult("request", DriverName, "channel-0", nil)
	claim := claimWithResults("claim-uid", result)

	_, err := state.applyComputeDomainChannelConfig(
		context.Background(),
		config,
		claim,
		[]*resourceapi.DeviceRequestAllocationResult{&result},
	)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "host nvidia-imex daemon readiness check failed")
	assert.False(t, isPermanentError(err), "an unready host daemon must be retried, not treated as permanently broken")
}

// TestApplyComputeDomainChannelConfigHostManagedCliqueIDRace guards against a
// race between periodicGPUCliqueIDRefresh's locked setCliqueID() writes and
// applyComputeDomainChannelConfigHostManaged's read of the clique ID. Run
// with -race.
func TestApplyComputeDomainChannelConfigHostManagedCliqueIDRace(t *testing.T) {
	cd := &configapi.ComputeDomain{
		ObjectMeta: metav1.ObjectMeta{Name: "cd", Namespace: "default", UID: "cd-uid"},
	}
	factory := nvinformers.NewSharedInformerFactory(nvfake.NewSimpleClientset(), 0)
	informer := factory.Resource().V1beta1().ComputeDomains().Informer()
	require.NoError(t, informer.AddIndexers(cache.Indexers{"computeDomainUID": uidIndexer[*configapi.ComputeDomain]}))
	require.NoError(t, informer.GetIndexer().Add(cd))

	state := &DeviceState{
		config:               hostManagedConfig(),
		checkpointManager:    &fakeCheckpointManager{checkpoint: checkpointWithClaims(nil)},
		computeDomainManager: &ComputeDomainManager{informer: informer},
		nvdevlib:             &deviceLib{devRoot: t.TempDir()},
		allocatable: AllocatableDevices{
			"channel-0": &AllocatableDevice{Channel: &ComputeDomainChannelInfo{ID: 0}},
		},
	}

	config := channelConfig("cd-uid")
	result := allocationResult("request", DriverName, "channel-0", nil)
	claim := claimWithResults("claim-uid", result)

	stop := make(chan struct{})
	var writerWG, readerWG sync.WaitGroup

	// Writer: mimics periodicGPUCliqueIDRefresh's locked cliqueID writes.
	writerWG.Go(func() {
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			state.computeDomainManager.setCliqueID(strconv.Itoa(i))
		}
	})

	// Reader: mimics a concurrent NodePrepareResources call hitting the host-managed channel config path.
	readerWG.Go(func() {
		for range 20000 {
			_, _ = state.applyComputeDomainChannelConfig(
				context.Background(),
				config,
				claim,
				[]*resourceapi.DeviceRequestAllocationResult{&result},
			)
		}
	})

	readerWG.Wait()
	close(stop)
	writerWG.Wait()
}

func TestUnprepareDevicesHostManagedSkipsRemoveNodeLabel(t *testing.T) {
	// computeDomainManager is intentionally nil: the channel branch must skip
	// RemoveNodeLabel under host-managed IMEX, so it must never be
	// dereferenced.
	state := testDeviceState()
	state.config = hostManagedConfig()
	status := claimStatus([]resourceapi.DeviceRequestAllocationResult{
		allocationResult("channel-request", DriverName, "channel-0", nil),
	})

	require.NoError(t, state.unprepareDevices(context.Background(), &status))
}

func testDeviceState() *DeviceState {
	return &DeviceState{
		allocatable: AllocatableDevices{
			"channel-0": &AllocatableDevice{Channel: &ComputeDomainChannelInfo{ID: 0}},
			"daemon-0":  &AllocatableDevice{Daemon: &ComputeDomainDaemonInfo{ID: 0}},
		},
	}
}

func testCheckpointDeviceState(checkpoint *Checkpoint) *DeviceState {
	return &DeviceState{
		checkpointManager: &fakeCheckpointManager{checkpoint: checkpoint},
		config: &Config{
			flags: &Flags{nodeName: "test-node"},
		},
	}
}

func requireFakeCheckpointManager(t *testing.T, state *DeviceState) *fakeCheckpointManager {
	t.Helper()

	manager, ok := state.checkpointManager.(*fakeCheckpointManager)
	require.True(t, ok)
	return manager
}

func channelConfig(domainID string) *configapi.ComputeDomainChannelConfig {
	config := configapi.DefaultComputeDomainChannelConfig()
	config.DomainID = domainID
	return config
}

func daemonConfig(domainID string) *configapi.ComputeDomainDaemonConfig {
	config := configapi.DefaultComputeDomainDaemonConfig()
	config.DomainID = domainID
	return config
}

func opaqueConfig(t *testing.T, source resourceapi.AllocationConfigSource, driver string, requests []string, obj runtime.Object) resourceapi.DeviceAllocationConfiguration {
	t.Helper()
	raw, err := json.Marshal(obj)
	require.NoError(t, err)

	return resourceapi.DeviceAllocationConfiguration{
		Source:   source,
		Requests: requests,
		DeviceConfiguration: resourceapi.DeviceConfiguration{
			Opaque: &resourceapi.OpaqueDeviceConfiguration{
				Driver: driver,
				Parameters: runtime.RawExtension{
					Raw: raw,
				},
			},
		},
	}
}

func allocationResult(request, driver, device string, adminAccess *bool) resourceapi.DeviceRequestAllocationResult {
	return resourceapi.DeviceRequestAllocationResult{
		Request:     request,
		Driver:      driver,
		Pool:        "pool",
		Device:      device,
		AdminAccess: adminAccess,
	}
}

func claimStatus(results []resourceapi.DeviceRequestAllocationResult, configs ...resourceapi.DeviceAllocationConfiguration) resourceapi.ResourceClaimStatus {
	return resourceapi.ResourceClaimStatus{
		Allocation: &resourceapi.AllocationResult{
			Devices: resourceapi.DeviceAllocationResult{
				Results: results,
				Config:  configs,
			},
		},
	}
}

func claimWithResults(uid string, results ...resourceapi.DeviceRequestAllocationResult) *resourceapi.ResourceClaim {
	return &resourceapi.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default",
			Name:      uid,
			UID:       types.UID(uid),
		},
		Status: claimStatus(results),
	}
}

func preparedClaim(state ClaimCheckpointState, results ...resourceapi.DeviceRequestAllocationResult) PreparedClaim {
	return PreparedClaim{
		CheckpointState: state,
		Status:          claimStatus(results),
	}
}

func checkpointWithClaims(claims map[string]PreparedClaim) *Checkpoint {
	if claims == nil {
		claims = make(map[string]PreparedClaim)
	}
	return &Checkpoint{
		V2: &CheckpointV2{
			PreparedClaims: claims,
		},
	}
}

func preparedChannel(id int) PreparedDevice {
	return PreparedDevice{
		Channel: &PreparedComputeDomainChannel{
			Info: &ComputeDomainChannelInfo{ID: id},
		},
	}
}

func preparedDaemon(id int) PreparedDevice {
	return PreparedDevice{
		Daemon: &PreparedComputeDomainDaemon{
			Info: &ComputeDomainDaemonInfo{ID: id},
		},
	}
}

func assertConfigResultDevices(t *testing.T, got map[runtime.Object][]*resourceapi.DeviceRequestAllocationResult, deviceType string, domainID string, expectedDevices []string) {
	t.Helper()

	for config, results := range got {
		if !configMatches(config, deviceType, domainID) {
			continue
		}
		var devices []string
		for _, result := range results {
			devices = append(devices, result.Device)
		}
		assert.ElementsMatch(t, expectedDevices, devices)
		return
	}

	require.Failf(t, "config result not found", "deviceType=%s domainID=%s", deviceType, domainID)
}

func assertNoConfigResult(t *testing.T, got map[runtime.Object][]*resourceapi.DeviceRequestAllocationResult, deviceType string, domainID string) {
	t.Helper()

	for config := range got {
		if configMatches(config, deviceType, domainID) {
			require.Failf(t, "unexpected config result found", "deviceType=%s domainID=%s", deviceType, domainID)
		}
	}
}

func configMatches(config runtime.Object, deviceType string, domainID string) bool {
	switch typed := config.(type) {
	case *configapi.ComputeDomainChannelConfig:
		return deviceType == ComputeDomainChannelType && typed.DomainID == domainID
	case *configapi.ComputeDomainDaemonConfig:
		return deviceType == ComputeDomainDaemonType && typed.DomainID == domainID
	default:
		return false
	}
}
