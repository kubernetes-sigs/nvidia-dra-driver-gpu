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
	"os"
	"path/filepath"
	"testing"

	"github.com/NVIDIA/nvidia-container-toolkit/pkg/nvcdi/spec"
	"github.com/stretchr/testify/require"

	utilcache "k8s.io/apimachinery/pkg/util/cache"
	cdiapi "tags.cncf.io/container-device-interface/pkg/cdi"
	cdispec "tags.cncf.io/container-device-interface/specs-go"
)

// fakeNvcdi is a minimal nvcdi.Interface that lets a test decide what
// GetDeviceSpecsByID() returns for a given UUID. Only the two methods that
// CreateClaimSpecFile() reaches are given behaviour.
type fakeNvcdi struct {
	commonEdits    *cdiapi.ContainerEdits
	deviceSpecs    []cdispec.Device
	deviceSpecsErr error
}

func (f *fakeNvcdi) GetCommonEdits() (*cdiapi.ContainerEdits, error) {
	return f.commonEdits, nil
}

func (f *fakeNvcdi) GetDeviceSpecsByID(...string) ([]cdispec.Device, error) {
	return f.deviceSpecs, f.deviceSpecsErr
}

func (f *fakeNvcdi) GetAllDeviceSpecs() ([]cdispec.Device, error) {
	return f.deviceSpecs, f.deviceSpecsErr
}

func (f *fakeNvcdi) GetSpec(...string) (spec.Interface, error) {
	return nil, errors.New("not implemented")
}

// fakePciDevicesRoot builds a stand-in for /sys/bus/pci/devices in which
// `pciAddress` is bound to `driver`. An empty driver creates the device
// directory without a `driver` symlink, which is what sysfs looks like for a
// device no driver has claimed.
func fakePciDevicesRoot(t *testing.T, pciAddress, driver string) string {
	t.Helper()

	root := t.TempDir()
	devicePath := filepath.Join(root, pciAddress)
	require.NoError(t, os.MkdirAll(devicePath, 0o755))

	if driver != "" {
		// Mirrors sysfs, where the `driver` entry is a relative symlink into
		// /sys/bus/pci/drivers. getDriver() only reads the link, so the target
		// does not have to exist.
		target := filepath.Join("..", "..", "drivers", driver)
		require.NoError(t, os.Symlink(target, filepath.Join(devicePath, "driver")))
	}

	return root
}

func TestDeviceBoundToVfio(t *testing.T) {
	const pciAddress = "0000:41:00.0"

	tests := map[string]struct {
		// driver is the driver the fake sysfs binds pciAddress to ("" for
		// none).
		driver string
		// queryAddress is the address deviceBoundToVfio is asked about.
		queryAddress string
		expected     bool
	}{
		"bound to vfio-pci": {
			driver:       "vfio-pci",
			queryAddress: pciAddress,
			expected:     true,
		},
		"bound to the Grace-Hopper vfio variant driver": {
			driver:       "nvgrace_gpu_vfio_pci",
			queryAddress: pciAddress,
			expected:     true,
		},
		"bound to nvidia": {
			driver:       nvidiaDriver,
			queryAddress: pciAddress,
			expected:     false,
		},
		"no driver bound": {
			driver:       "",
			queryAddress: pciAddress,
			expected:     false,
		},
		"device not present": {
			driver:       "vfio-pci",
			queryAddress: "0000:99:00.0",
			expected:     false,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			root := fakePciDevicesRoot(t, pciAddress, tc.driver)
			require.Equal(t, tc.expected, deviceBoundToVfio(root, tc.queryAddress))
		})
	}
}

// newTestCDIHandler builds a CDIHandler wired to a fake nvcdi library, a
// temporary CDI root and a fake sysfs, i.e. one that touches neither NVML nor
// the host filesystem.
func newTestCDIHandler(t *testing.T, nvcdiClaim *fakeNvcdi, pciDevicesRoot string) *CDIHandler {
	t.Helper()

	return &CDIHandler{
		nvcdiClaim:     nvcdiClaim,
		specCache:      utilcache.NewExpiring(),
		cdiRoot:        t.TempDir(),
		pciDevicesRoot: pciDevicesRoot,
	}
}

func gpuPreparedDevices(uuid, deviceName, pciBusID string) PreparedDevices {
	return PreparedDevices{
		{
			Devices: PreparedDeviceList{
				{
					Gpu: &PreparedGpu{
						Info:   &GpuInfo{UUID: uuid, pciBusID: pciBusID},
						Device: &CheckpointedDevice{DeviceName: deviceName},
					},
				},
			},
		},
	}
}

// readClaimSpec parses the transient CDI spec file CreateClaimSpecFile() wrote
// for claimUID.
func readClaimSpec(t *testing.T, cdi *CDIHandler, claimUID string) *cdispec.Spec {
	t.Helper()

	specName := cdiapi.GenerateTransientSpecName(cdiVendor, cdiClaimClass, claimUID)
	path := filepath.Join(cdi.cdiRoot, specName+".yaml")

	raw, err := cdiapi.ReadSpec(path, 0)
	require.NoError(t, err)

	return raw.Spec
}

// A GPU that a passthrough consumer already rebound to a vfio variant driver is
// invisible to NVML, so spec generation fails for it. Prepare must tolerate
// that instead of wedging the claim; see the comment in CreateClaimSpecFile().
func TestCreateClaimSpecFileToleratesVfioBoundGPU(t *testing.T) {
	const (
		claimUID   = "6b9b1e7a-1b3c-4d1e-9f0a-6d2b6a0f1c11"
		pciAddress = "0000:41:00.0"
		deviceName = "gpu-0"
	)

	for _, driver := range []string{"vfio-pci", "nvgrace_gpu_vfio_pci"} {
		t.Run(driver, func(t *testing.T) {
			cdi := newTestCDIHandler(t, &fakeNvcdi{
				commonEdits:    &cdiapi.ContainerEdits{ContainerEdits: &cdispec.ContainerEdits{}},
				deviceSpecsErr: errors.New("device handle from UUID: Not Found"),
			}, fakePciDevicesRoot(t, pciAddress, driver))

			err := cdi.CreateClaimSpecFile(claimUID, gpuPreparedDevices("GPU-fake", deviceName, pciAddress))
			require.NoError(t, err)

			// The claim must still get a spec, and the device in it must carry
			// container edits: CDI rejects a device with empty edits.
			s := readClaimSpec(t, cdi, claimUID)
			require.Len(t, s.Devices, 1)
			require.Equal(t, claimUID+"-"+deviceName, s.Devices[0].Name)
			require.Equal(t, []string{"NVIDIA_DRA_GPU_VFIO_PASSTHROUGH=" + pciAddress}, s.Devices[0].ContainerEdits.Env)
			// No /dev/nvidia* nodes: the consumer attaches the GPU by PCI
			// address.
			require.Empty(t, s.Devices[0].ContainerEdits.DeviceNodes)
		})
	}
}

// The toleration is narrow: a spec generation failure for a GPU that is not
// vfio-bound is a genuine error and must still be surfaced.
func TestCreateClaimSpecFileSurfacesErrorForNonVfioGPU(t *testing.T) {
	const (
		claimUID   = "6b9b1e7a-1b3c-4d1e-9f0a-6d2b6a0f1c11"
		pciAddress = "0000:41:00.0"
		deviceName = "gpu-0"
	)

	tests := map[string]struct {
		// driver is the driver the GPU is bound to in the fake sysfs.
		driver string
		// pciBusID is what the prepared device reports; empty means the bus ID
		// was never discovered, so no binding check is possible.
		pciBusID string
	}{
		"bound to nvidia":      {driver: nvidiaDriver, pciBusID: pciAddress},
		"no driver bound":      {driver: "", pciBusID: pciAddress},
		"pci bus ID not known": {driver: "vfio-pci", pciBusID: ""},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			cdi := newTestCDIHandler(t, &fakeNvcdi{
				commonEdits:    &cdiapi.ContainerEdits{ContainerEdits: &cdispec.ContainerEdits{}},
				deviceSpecsErr: errors.New("device handle from UUID: Not Found"),
			}, fakePciDevicesRoot(t, pciAddress, tc.driver))

			err := cdi.CreateClaimSpecFile(claimUID, gpuPreparedDevices("GPU-fake", deviceName, tc.pciBusID))

			require.ErrorContains(t, err, "unable to get device spec for "+claimUID+"-"+deviceName)
			require.ErrorContains(t, err, "device handle from UUID: Not Found")
		})
	}
}

// A GPU NVML can see must be unaffected by the vfio toleration.
func TestCreateClaimSpecFileUsesNvcdiSpecForNvmlVisibleGPU(t *testing.T) {
	const (
		claimUID   = "6b9b1e7a-1b3c-4d1e-9f0a-6d2b6a0f1c11"
		pciAddress = "0000:41:00.0"
		deviceName = "gpu-0"
	)

	cdi := newTestCDIHandler(t, &fakeNvcdi{
		commonEdits: &cdiapi.ContainerEdits{ContainerEdits: &cdispec.ContainerEdits{}},
		deviceSpecs: []cdispec.Device{
			{
				Name: "placeholder",
				ContainerEdits: cdispec.ContainerEdits{
					DeviceNodes: []*cdispec.DeviceNode{{Path: "/dev/nvidia0"}},
				},
			},
		},
	}, fakePciDevicesRoot(t, pciAddress, nvidiaDriver))

	err := cdi.CreateClaimSpecFile(claimUID, gpuPreparedDevices("GPU-fake", deviceName, pciAddress))
	require.NoError(t, err)

	s := readClaimSpec(t, cdi, claimUID)
	require.Len(t, s.Devices, 1)
	require.Equal(t, claimUID+"-"+deviceName, s.Devices[0].Name)
	require.Len(t, s.Devices[0].ContainerEdits.DeviceNodes, 1)
	require.Equal(t, "/dev/nvidia0", s.Devices[0].ContainerEdits.DeviceNodes[0].Path)
	require.Empty(t, s.Devices[0].ContainerEdits.Env)
}
