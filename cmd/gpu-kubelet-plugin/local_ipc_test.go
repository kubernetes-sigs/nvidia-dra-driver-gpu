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
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"sigs.k8s.io/dra-driver-nvidia-gpu/pkg/featuregates"
)

func TestLocalIPCManager(t *testing.T) {
	manager := NewLocalIPCManager(t.TempDir())

	info, err := manager.ensureDirectory("claim-1")
	require.NoError(t, err)
	require.Equal(t, localIPCContainerPath, info.ContainerPath)

	stat, err := os.Stat(info.HostPath)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o777), stat.Mode().Perm())
	assert.NotZero(t, stat.Mode()&os.ModeSticky)

	marker := filepath.Join(info.HostPath, "marker")
	require.NoError(t, os.WriteFile(marker, []byte("preserved"), 0o600))
	_, err = manager.ensureDirectory("claim-1")
	require.NoError(t, err)
	_, err = os.Stat(marker)
	require.NoError(t, err)

	otherInfo, err := manager.ensureDirectory("claim-other")
	require.NoError(t, err)
	assert.NotEqual(t, info.HostPath, otherInfo.HostPath)
	_, err = os.Stat(filepath.Join(otherInfo.HostPath, "marker"))
	require.ErrorIs(t, err, os.ErrNotExist)

	for _, claimUID := range []string{"", ".", "..", "../escape", "nested/claim"} {
		_, err = manager.ensureDirectory(claimUID)
		require.Errorf(t, err, "claim UID %q must be rejected", claimUID)
	}

	claimSymlinkTarget := t.TempDir()
	claimSymlinkMarker := filepath.Join(claimSymlinkTarget, "marker")
	require.NoError(t, os.WriteFile(claimSymlinkMarker, []byte("preserved"), 0o600))
	require.NoError(t, os.Symlink(claimSymlinkTarget, filepath.Join(manager.root, "claim-2")))
	_, err = manager.ensureDirectory("claim-2")
	require.Error(t, err)
	require.NoError(t, manager.Remove("claim-2"))
	_, err = os.Stat(claimSymlinkMarker)
	require.NoError(t, err)

	symlinkRootManager := NewLocalIPCManager(t.TempDir())
	symlinkRootTarget := t.TempDir()
	targetClaimPath := filepath.Join(symlinkRootTarget, "claim-3")
	require.NoError(t, os.Mkdir(targetClaimPath, 0o700))
	require.NoError(t, os.Symlink(symlinkRootTarget, symlinkRootManager.root))
	_, err = symlinkRootManager.ensureDirectory("claim-3")
	require.Error(t, err)
	require.Error(t, symlinkRootManager.Remove("claim-3"))
	require.Error(t, symlinkRootManager.Reconcile(nil))
	_, err = os.Stat(targetClaimPath)
	require.NoError(t, err)

	require.NoError(t, manager.Remove("claim-1"))
	_, err = os.Stat(info.HostPath)
	require.ErrorIs(t, err, os.ErrNotExist)
	_, err = os.Stat(otherInfo.HostPath)
	require.NoError(t, err)
}

func TestLocalIPCReconcile(t *testing.T) {
	emptyManager := NewLocalIPCManager(t.TempDir())
	require.NoError(t, emptyManager.Reconcile(nil))
	_, err := os.Stat(emptyManager.root)
	require.ErrorIs(t, err, os.ErrNotExist)

	manager := NewLocalIPCManager(t.TempDir())
	for _, claimUID := range []string{"active", "stale", "partial", "disabled"} {
		_, err = manager.ensureDirectory(claimUID)
		require.NoError(t, err)
	}
	activeMarker := filepath.Join(manager.root, "active", "marker")
	require.NoError(t, os.WriteFile(activeMarker, []byte("preserved"), 0o600))

	claims := PreparedClaimsByUID{
		"active": {
			CheckpointState: ClaimCheckpointStatePrepareCompleted,
			Status: resourceapi.ResourceClaimStatus{
				Allocation: allocationWithLocalIPCConfig(),
			},
		},
		"partial": {
			CheckpointState: ClaimCheckpointStatePrepareStarted,
			Status: resourceapi.ResourceClaimStatus{
				Allocation: allocationWithLocalIPCConfig(),
			},
		},
		"disabled": {
			CheckpointState: ClaimCheckpointStatePrepareCompleted,
			Status: resourceapi.ResourceClaimStatus{
				Allocation: allocationWithLocalIPCConfigEnabled(false),
			},
		},
	}
	require.NoError(t, manager.Reconcile(claims))

	_, err = os.Stat(activeMarker)
	require.NoError(t, err)
	for _, claimUID := range []string{"stale", "partial", "disabled"} {
		_, err = os.Stat(filepath.Join(manager.root, claimUID))
		require.ErrorIs(t, err, os.ErrNotExist)
	}
}

func TestLocalIPCInfoGetCDIContainerEdits(t *testing.T) {
	info := &LocalIPCInfo{HostPath: "/host/claim", ContainerPath: localIPCContainerPath}

	edits := info.GetCDIContainerEdits()

	require.Equal(t, []string{"NVIDIA_DRA_LOCAL_IPC_DIR=" + localIPCContainerPath}, edits.Env)
	require.Len(t, edits.Mounts, 1)
	assert.Equal(t, info.HostPath, edits.Mounts[0].HostPath)
	assert.Equal(t, info.ContainerPath, edits.Mounts[0].ContainerPath)
	assert.Equal(t, []string{"rw", "nosuid", "nodev", "noexec", "bind"}, edits.Mounts[0].Options)
}

func TestGetLocalIPCConfig(t *testing.T) {
	old := featuregates.Enabled(featuregates.LocalIPCDirectory)
	t.Cleanup(func() {
		require.NoError(t, featuregates.FeatureGates().SetFromMap(map[string]bool{
			string(featuregates.LocalIPCDirectory): old,
		}))
	})
	require.NoError(t, featuregates.FeatureGates().SetFromMap(map[string]bool{
		string(featuregates.LocalIPCDirectory): true,
	}))

	config := localIPCAllocationConfig
	allocation := func(configs ...resourceapi.DeviceAllocationConfiguration) *resourceapi.AllocationResult {
		return &resourceapi.AllocationResult{
			Devices: resourceapi.DeviceAllocationResult{Config: configs},
		}
	}

	got, err := getLocalIPCConfig(allocation(config()))
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.True(t, got.Enabled)

	manager := NewLocalIPCManager(t.TempDir())
	info, err := manager.Prepare("prepared", allocation(config()))
	require.NoError(t, err)
	require.NotNil(t, info)

	_, err = getLocalIPCConfig(allocation(config("gpu")))
	require.ErrorContains(t, err, "must not select individual requests")

	classConfig := config()
	classConfig.Source = resourceapi.AllocationConfigSourceClass
	got, err = getLocalIPCConfig(allocation(classConfig, localIPCAllocationConfigWithEnabled(false)))
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.False(t, got.Enabled)
	disabledInfo, err := manager.Prepare("disabled", allocation(classConfig, localIPCAllocationConfigWithEnabled(false)))
	require.NoError(t, err)
	require.Nil(t, disabledInfo)
	_, err = os.Stat(filepath.Join(manager.root, "disabled"))
	require.ErrorIs(t, err, os.ErrNotExist)

	futureConfig := config()
	futureConfig.Opaque.Parameters.Raw = []byte(`{
		"apiVersion": "resource.nvidia.com/v1beta1",
		"kind": "LocalIPCConfig",
		"enabled": true,
		"futureOption": "value"
	}`)
	_, err = getLocalIPCConfig(allocation(futureConfig))
	require.Error(t, err)

	got, err = getCheckpointLocalIPCConfig(allocation(futureConfig))
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.True(t, got.Enabled)

	unrelatedFutureConfig := config()
	unrelatedFutureConfig.Opaque.Parameters.Raw = []byte(`{
		"apiVersion": "resource.nvidia.com/v1beta1",
		"kind": "FutureConfig",
		"futureOption": "value"
	}`)
	unrelatedFutureConfig.Source = resourceapi.AllocationConfigSource("FromFuture")
	got, err = getCheckpointLocalIPCConfig(allocation(unrelatedFutureConfig, config()))
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.True(t, got.Enabled)

	require.NoError(t, featuregates.FeatureGates().SetFromMap(map[string]bool{
		string(featuregates.LocalIPCDirectory): false,
	}))
	_, err = manager.Prepare("rejected", allocation(config()))
	require.Error(t, err)
	require.NoError(t, manager.Restore("restored", allocation(futureConfig)))
	_, err = os.Stat(filepath.Join(manager.root, "restored"))
	require.NoError(t, err)
}

func allocationWithLocalIPCConfig() *resourceapi.AllocationResult {
	return allocationWithLocalIPCConfigEnabled(true)
}

func allocationWithLocalIPCConfigEnabled(enabled bool) *resourceapi.AllocationResult {
	return &resourceapi.AllocationResult{
		Devices: resourceapi.DeviceAllocationResult{
			Config: []resourceapi.DeviceAllocationConfiguration{
				localIPCAllocationConfigWithEnabled(enabled),
			},
		},
	}
}

func localIPCAllocationConfig(requests ...string) resourceapi.DeviceAllocationConfiguration {
	return localIPCAllocationConfigWithEnabled(true, requests...)
}

func localIPCAllocationConfigWithEnabled(enabled bool, requests ...string) resourceapi.DeviceAllocationConfiguration {
	return resourceapi.DeviceAllocationConfiguration{
		Source:   resourceapi.AllocationConfigSourceClaim,
		Requests: requests,
		DeviceConfiguration: resourceapi.DeviceConfiguration{
			Opaque: &resourceapi.OpaqueDeviceConfiguration{
				Driver: DriverName,
				Parameters: runtime.RawExtension{Raw: []byte(fmt.Sprintf(`{
					"apiVersion": "resource.nvidia.com/v1beta1",
					"kind": "LocalIPCConfig",
					"enabled": %t
				}`, enabled))},
			},
		},
	}
}
