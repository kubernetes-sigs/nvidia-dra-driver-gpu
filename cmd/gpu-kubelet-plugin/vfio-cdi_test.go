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
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/NVIDIA/go-nvlib/pkg/nvpci"
	"github.com/stretchr/testify/require"
)

func TestNewVfioCDIHandler(t *testing.T) {
	t.Run("iommufd disabled when /dev/iommu missing", func(t *testing.T) {
		handler, err := NewVfioCDIHandler(&deviceLib{hostRoot: t.TempDir()})
		require.NoError(t, err)
		require.False(t, handler.iommuFDEnabled)
	})

	t.Run("iommufd enabled when /dev/iommu exists", func(t *testing.T) {
		hostRoot := t.TempDir()
		require.NoError(t, os.MkdirAll(filepath.Join(hostRoot, "dev"), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(hostRoot, iommuDevicePath), nil, 0o644))

		handler, err := NewVfioCDIHandler(&deviceLib{hostRoot: hostRoot})
		require.NoError(t, err)
		require.True(t, handler.iommuFDEnabled)
	})
}

func TestVfioCDIHandlerGetCommonEdits(t *testing.T) {
	testCases := []struct {
		name            string
		enableAPIDevice bool
		preferIommuFD   bool
		iommuFDEnabled  bool
		expectedPaths   []string
	}{
		{
			name:            "api device disabled",
			enableAPIDevice: false,
			preferIommuFD:   true,
			iommuFDEnabled:  true,
			expectedPaths:   []string{},
		},
		{
			name:            "iommufd preferred and enabled",
			enableAPIDevice: true,
			preferIommuFD:   true,
			iommuFDEnabled:  true,
			expectedPaths:   []string{iommuDevicePath},
		},
		{
			name:            "iommufd preferred but not enabled",
			enableAPIDevice: true,
			preferIommuFD:   true,
			iommuFDEnabled:  false,
			expectedPaths:   []string{"/dev/vfio/vfio"},
		},
		{
			name:            "iommufd enabled but not preferred",
			enableAPIDevice: true,
			preferIommuFD:   false,
			iommuFDEnabled:  true,
			expectedPaths:   []string{"/dev/vfio/vfio"},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			handler := &vfioCDIHandler{iommuFDEnabled: tc.iommuFDEnabled}

			edits, err := handler.GetCommonEdits(tc.enableAPIDevice, tc.preferIommuFD)
			require.NoError(t, err)
			require.Equal(t, []string{"NVIDIA_VISIBLE_DEVICES=void"}, edits.Env)

			paths := []string{}
			for _, node := range edits.DeviceNodes {
				paths = append(paths, node.Path)
			}
			require.Equal(t, tc.expectedPaths, paths)
		})
	}
}

func TestVfioCDIHandlerGetDeviceSpecsByPCIBusID(t *testing.T) {
	testCases := []struct {
		name           string
		preferIommuFD  bool
		iommuFDEnabled bool
		device         *nvpci.NvidiaPCIDevice
		deviceErr      error
		expectedPath   string
		expectedErr    string
	}{
		{
			name:           "legacy vfio group device",
			preferIommuFD:  false,
			iommuFDEnabled: true,
			device:         &nvpci.NvidiaPCIDevice{Address: "0000:01:00.0", IommuGroup: 42},
			expectedPath:   "/dev/vfio/42",
		},
		{
			name:           "iommufd cdev",
			preferIommuFD:  true,
			iommuFDEnabled: true,
			device:         &nvpci.NvidiaPCIDevice{Address: "0000:01:00.0", IommuFD: "vfio0"},
			expectedPath:   "/dev/vfio/devices/vfio0",
		},
		{
			name:           "iommufd cdev missing",
			preferIommuFD:  true,
			iommuFDEnabled: true,
			device:         &nvpci.NvidiaPCIDevice{Address: "0000:01:00.0"},
			expectedErr:    "missing iommufd cdev",
		},
		{
			name:        "pci lookup error",
			deviceErr:   errors.New("device not found"),
			expectedErr: "error getting PCI device info",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			handler := &vfioCDIHandler{
				iommuFDEnabled: tc.iommuFDEnabled,
				deviceLib: &deviceLib{
					nvpci: &nvpci.InterfaceMock{
						GetGPUByPciBusIDFunc: func(s string) (*nvpci.NvidiaPCIDevice, error) {
							return tc.device, tc.deviceErr
						},
					},
				},
			}

			specs, err := handler.GetDeviceSpecsByPCIBusID("0000:01:00.0", tc.preferIommuFD)
			if tc.expectedErr != "" {
				require.ErrorContains(t, err, tc.expectedErr)
				return
			}
			require.NoError(t, err)
			require.Len(t, specs, 1)
			require.Len(t, specs[0].ContainerEdits.DeviceNodes, 1)
			require.Equal(t, tc.expectedPath, specs[0].ContainerEdits.DeviceNodes[0].Path)
		})
	}
}
