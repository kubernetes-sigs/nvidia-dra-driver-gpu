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
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/NVIDIA/nvidia-container-toolkit/pkg/nvcdi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	utilcache "k8s.io/apimachinery/pkg/util/cache"
	cdiapi "tags.cncf.io/container-device-interface/pkg/cdi"
	cdispec "tags.cncf.io/container-device-interface/specs-go"
)

type fakeNVCDI struct {
	nvcdi.Interface

	commonEditsCalls int
	commonEdits      *cdiapi.ContainerEdits
	commonEditsErr   error

	deviceSpecsCalls map[string]int
	deviceSpecs      map[string][]cdispec.Device
	deviceSpecsErrs  map[string]error
}

func (f *fakeNVCDI) GetCommonEdits() (*cdiapi.ContainerEdits, error) {
	f.commonEditsCalls++
	return f.commonEdits, f.commonEditsErr
}

func (f *fakeNVCDI) GetDeviceSpecsByID(ids ...string) ([]cdispec.Device, error) {
	uuid := ids[0]
	f.deviceSpecsCalls[uuid]++
	if err := f.deviceSpecsErrs[uuid]; err != nil {
		return nil, err
	}
	return f.deviceSpecs[uuid], nil
}

func TestGetCommonEditsCached(t *testing.T) {
	newCommonEdits := func() *cdiapi.ContainerEdits {
		return &cdiapi.ContainerEdits{
			ContainerEdits: &cdispec.ContainerEdits{
				Env: []string{"FOO=bar"},
			},
		}
	}

	tests := map[string]struct {
		commonEdits        *cdiapi.ContainerEdits
		commonEditsErr     error
		cachedValue        any
		callCount          int
		wantUnderlyingCall int
		wantErr            string
		checkResults       func(*testing.T, []*cdiapi.ContainerEdits)
	}{
		"cache miss followed by cache hit": {
			commonEdits:        newCommonEdits(),
			callCount:          2,
			wantUnderlyingCall: 1,
			checkResults: func(t *testing.T, results []*cdiapi.ContainerEdits) {
				require.Len(t, results, 2)
				assert.NotSame(t, results[0], results[1])
				assert.Equal(t, results[0], results[1])
			},
		},
		"underlying errors are not cached": {
			commonEditsErr:     errors.New("mock error"),
			callCount:          2,
			wantUnderlyingCall: 2,
			wantErr:            "mock error",
		},
		"invalid cached value returns an error": {
			cachedValue:        "invalid value",
			callCount:          1,
			wantUnderlyingCall: 0,
			wantErr:            "expected *cdiapi.ContainerEdits",
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			fakeNVCDIClaim := &fakeNVCDI{
				commonEdits:    tc.commonEdits,
				commonEditsErr: tc.commonEditsErr,
			}
			handler := &CDIHandler{
				nvcdiClaim: fakeNVCDIClaim,
				specCache:  utilcache.NewExpiring(),
			}

			if tc.cachedValue != nil {
				handler.specCache.Set(
					"commonEdits",
					tc.cachedValue,
					5*time.Minute,
				)
			}

			var results []*cdiapi.ContainerEdits
			for range tc.callCount {
				got, err := handler.GetCommonEditsCached()

				if tc.wantErr != "" {
					require.Error(t, err)
					assert.Contains(t, err.Error(), tc.wantErr)
					continue
				}

				require.NoError(t, err)
				results = append(results, got)
			}

			assert.Equal(
				t,
				tc.wantUnderlyingCall,
				fakeNVCDIClaim.commonEditsCalls,
			)

			if tc.checkResults != nil {
				tc.checkResults(t, results)
			}
		})
	}
}

func TestGetDeviceSpecsByUUIDCached(t *testing.T) {
	tests := map[string]struct {
		deviceSpecs        map[string][]cdispec.Device
		deviceSpecsErrs    map[string]error
		cachedValues       map[string]any
		requestUUIDs       []string
		mutateFirstResult  bool
		wantUnderlyingCall map[string]int
		wantErr            string
		checkResults       func(*testing.T, [][]cdispec.Device)
	}{
		"cache miss followed by cache hit": {
			deviceSpecs: map[string][]cdispec.Device{
				"GPU-123": {
					{Name: "original-name"},
				},
			},
			requestUUIDs:      []string{"GPU-123", "GPU-123"},
			mutateFirstResult: true,
			wantUnderlyingCall: map[string]int{
				"GPU-123": 1,
			},
			checkResults: func(t *testing.T, results [][]cdispec.Device) {
				require.Len(t, results, 2)
				require.Len(t, results[1], 1)
				assert.Equal(t, "original-name", results[1][0].Name)
			},
		},
		"entries are cached independently by UUID": {
			deviceSpecs: map[string][]cdispec.Device{
				"GPU-1": {
					{Name: "gpu-1"},
				},
				"GPU-2": {
					{Name: "gpu-2"},
				},
			},
			requestUUIDs: []string{
				"GPU-1",
				"GPU-2",
				"GPU-1",
				"GPU-2",
			},
			wantUnderlyingCall: map[string]int{
				"GPU-1": 1,
				"GPU-2": 1,
			},
			checkResults: func(t *testing.T, results [][]cdispec.Device) {
				require.Len(t, results, 4)
				assert.Equal(t, "gpu-1", results[0][0].Name)
				assert.Equal(t, "gpu-2", results[1][0].Name)
				assert.Equal(t, "gpu-1", results[2][0].Name)
				assert.Equal(t, "gpu-2", results[3][0].Name)
			},
		},
		"underlying errors are not cached": {
			deviceSpecs: map[string][]cdispec.Device{},
			deviceSpecsErrs: map[string]error{
				"GPU-123": errors.New("mock error"),
			},
			requestUUIDs: []string{"GPU-123", "GPU-123"},
			wantUnderlyingCall: map[string]int{
				"GPU-123": 2,
			},
			wantErr: "mock error",
		},
		"invalid cached value returns an error": {
			deviceSpecs: map[string][]cdispec.Device{},
			cachedValues: map[string]any{
				"GPU-123": "invalid value",
			},
			requestUUIDs:       []string{"GPU-123"},
			wantUnderlyingCall: map[string]int{},
			wantErr:            "expected []cdispec.Device",
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			fakeNVCDIClaim := &fakeNVCDI{
				deviceSpecsCalls: map[string]int{},
				deviceSpecs:      tc.deviceSpecs,
				deviceSpecsErrs:  tc.deviceSpecsErrs,
			}
			handler := &CDIHandler{
				nvcdiClaim: fakeNVCDIClaim,
				specCache:  utilcache.NewExpiring(),
			}

			for uuid, value := range tc.cachedValues {
				handler.specCache.Set(uuid, value, 5*time.Minute)
			}

			var results [][]cdispec.Device
			for i, uuid := range tc.requestUUIDs {
				got, err := handler.GetDeviceSpecsByUUIDCached(uuid)

				if tc.wantErr != "" {
					require.Error(t, err)
					assert.Contains(t, err.Error(), tc.wantErr)
					continue
				}

				require.NoError(t, err)

				if i == 0 && tc.mutateFirstResult {
					require.NotEmpty(t, got)
					got[0].Name = "mutated-name"
				}

				results = append(results, got)
			}

			assert.Equal(
				t,
				tc.wantUnderlyingCall,
				fakeNVCDIClaim.deviceSpecsCalls,
			)

			if tc.checkResults != nil {
				tc.checkResults(t, results)
			}
		})
	}
}

func TestWarmupDevSpecCache(t *testing.T) {
	fakeNVCDIClaim := &fakeNVCDI{
		deviceSpecsCalls: map[string]int{},
		deviceSpecs: map[string][]cdispec.Device{
			"GPU-1": {{Name: "gpu-1"}},
			"GPU-2": {{Name: "gpu-2"}},
		},
		deviceSpecsErrs: map[string]error{
			"GPU-error": errors.New("mock error"),
		},
	}
	handler := &CDIHandler{
		nvcdiClaim: fakeNVCDIClaim,
		specCache:  utilcache.NewExpiring(),
	}

	handler.WarmupDevSpecCache([]string{
		"GPU-1",
		"GPU-error",
		"GPU-2",
		"GPU-1",
	})

	for uuid, wantName := range map[string]string{
		"GPU-1": "gpu-1",
		"GPU-2": "gpu-2",
	} {
		got, err := handler.GetDeviceSpecsByUUIDCached(uuid)
		require.NoError(t, err)
		require.Len(t, got, 1)
		assert.Equal(t, wantName, got[0].Name)
	}

	assert.Equal(t, map[string]int{
		"GPU-1":     1,
		"GPU-2":     1,
		"GPU-error": 1,
	}, fakeNVCDIClaim.deviceSpecsCalls)
}

func TestCreateClaimSpecFile(t *testing.T) {
	const (
		claimUID = "claim-123"
		uuid     = "GPU-123"
	)

	tests := map[string]struct {
		commonEditsErr      error
		deviceSpecsErrs     map[string]error
		wantErr             string
		wantCommonEditCalls int
		wantDeviceSpecCalls map[string]int
	}{
		"regular GPU": {
			wantCommonEditCalls: 1,
			wantDeviceSpecCalls: map[string]int{uuid: 1},
		},
		"common edits error": {
			commonEditsErr:      errors.New("mock common edits error"),
			wantErr:             "failed to get common CDI spec edits: mock common edits error",
			wantCommonEditCalls: 1,
			wantDeviceSpecCalls: map[string]int{},
		},
		"device spec error": {
			deviceSpecsErrs: map[string]error{
				uuid: errors.New("mock device spec error"),
			},
			wantErr:             "unable to get device spec for claim-123-gpu-0: mock device spec error",
			wantCommonEditCalls: 1,
			wantDeviceSpecCalls: map[string]int{uuid: 1},
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			fakeNVCDIClaim := &fakeNVCDI{
				commonEdits: &cdiapi.ContainerEdits{
					ContainerEdits: &cdispec.ContainerEdits{
						Env: []string{"COMMON=value"},
					},
				},
				commonEditsErr:   tc.commonEditsErr,
				deviceSpecsCalls: map[string]int{},
				deviceSpecs: map[string][]cdispec.Device{
					uuid: {
						{
							ContainerEdits: cdispec.ContainerEdits{
								Env: []string{"DEVICE=value"},
							},
						},
					},
				},
				deviceSpecsErrs: tc.deviceSpecsErrs,
			}
			cdiRoot := t.TempDir()
			handler := &CDIHandler{
				nvcdiClaim: fakeNVCDIClaim,
				specCache:  utilcache.NewExpiring(),
				cdiRoot:    cdiRoot,
			}
			preparedDevices := PreparedDevices{
				{
					Devices: PreparedDeviceList{
						{
							Gpu: &PreparedGpu{
								Info: &GpuInfo{UUID: uuid},
								Device: &CheckpointedDevice{
									DeviceName: "gpu-0",
								},
							},
						},
					},
					ConfigState: DeviceConfigState{
						containerEdits: &cdiapi.ContainerEdits{
							ContainerEdits: &cdispec.ContainerEdits{
								Env: []string{"GROUP=value"},
							},
						},
					},
				},
			}
			specName := cdiapi.GenerateTransientSpecName(cdiVendor, cdiClaimClass, claimUID)
			specPath := filepath.Join(cdiRoot, specName+".yaml")

			err := handler.CreateClaimSpecFile(claimUID, preparedDevices)
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				require.NoFileExists(t, specPath)
			} else {
				require.NoError(t, err)

				generated, err := cdiapi.ReadSpec(specPath, 0)
				require.NoError(t, err)
				assert.Equal(t, cdiVendor+"/"+cdiClaimClass, generated.Kind)
				assert.Equal(t, []string{"COMMON=value"}, generated.ContainerEdits.Env)
				require.Len(t, generated.Devices, 1)
				assert.Equal(t, "claim-123-gpu-0", generated.Devices[0].Name)
				assert.Equal(t, []string{"DEVICE=value", "GROUP=value"}, generated.Devices[0].ContainerEdits.Env)
			}

			assert.Equal(t, tc.wantCommonEditCalls, fakeNVCDIClaim.commonEditsCalls)
			assert.Equal(t, tc.wantDeviceSpecCalls, fakeNVCDIClaim.deviceSpecsCalls)
		})
	}
}
