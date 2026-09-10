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
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/klog/v2"

	nvdev "github.com/NVIDIA/go-nvlib/pkg/nvlib/device"
	"github.com/NVIDIA/go-nvlib/pkg/nvpassthrough"
	"github.com/NVIDIA/go-nvlib/pkg/nvpci"
	"github.com/NVIDIA/go-nvml/pkg/nvml"
	"k8s.io/dynamic-resource-allocation/deviceattribute"

	"sigs.k8s.io/dra-driver-nvidia-gpu/internal/lookup/root"
	"sigs.k8s.io/dra-driver-nvidia-gpu/pkg/featuregates"
)

type GPUMinor = int

type deviceLib struct {
	nvdev.Interface
	nvmllib           nvml.Interface
	nvpci             nvpci.Interface
	nvpasst           nvpassthrough.Interface
	driverLibraryPath string
	devRoot           string
	hostRoot          string
	sysfsRoot         string
	nvidiaSMIPath     string
	// vfioEnabled records node-local IOMMU capability, independent of feature gates.
	vfioEnabled       bool
	gpuInfosByUUID    map[string]*GpuInfo
	gpuUUIDbyPCIBusID map[PCIBusID]string
	devhandleByUUID   map[string]nvml.Device
}

func newDeviceLib(driver *root.Driver, hostRoot string) (*deviceLib, error) {
	vfioEnabled, err := checkIommuEnabled(hostRoot)
	if err != nil {
		return nil, fmt.Errorf("error checking if IOMMU is enabled: %w", err)
	}

	driverLibraryPath, err := driver.DriverLibraryPath()
	if err != nil {
		return nil, fmt.Errorf("failed to locate driver libraries: %w", err)
	}

	nvidiaSMIPath, err := driver.BinaryPath("nvidia-smi")
	if err != nil {
		return nil, fmt.Errorf("failed to locate nvidia-smi: %w", err)
	}

	// We construct an NVML library specifying the path to libnvidia-ml.so.1
	// explicitly so that we don't have to rely on the library path.
	nvmllib := nvml.New(
		nvml.WithLibraryPath(driverLibraryPath),
	)
	sysfsRoot := strings.TrimSpace(os.Getenv("NVIDIA_DRA_SYSFS_ROOT"))
	if sysfsRoot != "" {
		klog.Infof("Using alternate sysfs root: %s", sysfsRoot)
	}
	nvpci := nvpci.New()
	nvpassthrough := nvpassthrough.New(
		nvpassthrough.WithNvpciLib(nvpci),
		nvpassthrough.WithHostRoot(hostRoot),
	)

	d := deviceLib{
		Interface:         nvdev.New(nvmllib),
		nvmllib:           nvmllib,
		driverLibraryPath: driverLibraryPath,
		devRoot:           driver.DevRoot,
		hostRoot:          hostRoot,
		vfioEnabled:       vfioEnabled,
		sysfsRoot:         sysfsRoot,
		nvidiaSMIPath:     nvidiaSMIPath,
		nvpci:             nvpci,
		nvpasst:           nvpassthrough,
		gpuInfosByUUID:    make(map[string]*GpuInfo),
		gpuUUIDbyPCIBusID: make(map[PCIBusID]string),
		devhandleByUUID:   make(map[string]nvml.Device),
	}

	// Current design: when DynamicMIG is enabled, use one long-lived NVML
	// session.
	if featuregates.Enabled(featuregates.DynamicMIG) {
		klog.V(1).Infof("DynamicMIG enabled: initialize long-lived NVML session")
		if err := d.Init(); err != nil {
			return nil, fmt.Errorf("failed to initialize NVML: %w", err)
		}
	}

	return &d, nil
}

func (d *deviceLib) IsVfioEnabled() bool {
	return d.vfioEnabled
}

// prependPathListEnvvar prepends a specified list of strings to a specified envvar and returns its value.
func prependPathListEnvvar(envvar string, prepend ...string) string {
	if len(prepend) == 0 {
		return os.Getenv(envvar)
	}
	current := filepath.SplitList(os.Getenv(envvar))
	return strings.Join(append(prepend, current...), string(filepath.ListSeparator))
}

// setOrOverrideEnvvar adds or updates an envar to the list of specified envvars and returns it.
func setOrOverrideEnvvar(envvars []string, key, value string) []string {
	var updated []string
	for _, envvar := range envvars {
		pair := strings.SplitN(envvar, "=", 2)
		if pair[0] == key {
			continue
		}
		updated = append(updated, envvar)
	}
	return append(updated, fmt.Sprintf("%s=%s", key, value))
}

// Getting a device handle by UUID can take O(10 s) when done concurrently. For
// faster device management, maintain long-term state: initialize once at
// startup, cache handles, serialize NVML calls. TODO: implement a re-init path
// on NVML errors; this hopefully balances performance and robustness for this
// long-running process. Downside: out-of-band NVML clients (e.g., mig-parted)
// may see "in use by another client" errors (they are forbidden by design when
// DynamicMIG is enabled).
func (l deviceLib) Init() error {
	klog.V(6).Infof("Call NVML Init")
	// Its possible there are no GPUs available in NVML.
	// (Eg: All gpus prepared in passthrough-mode)
	// We use the INIT_FLAG_NO_GPUS flag to avoid failing if there are no GPUs.
	ret := l.nvmllib.InitWithFlags(nvml.INIT_FLAG_NO_GPUS)
	if ret != nvml.SUCCESS {
		return fmt.Errorf("error initializing NVML: %w", ret)
	}
	return nil
}

func (l deviceLib) alwaysShutdown() {
	klog.V(6).Infof("Call NVML shutdown")
	ret := l.nvmllib.Shutdown()
	if ret != nvml.SUCCESS {
		klog.Warningf("error shutting down NVML: %v", ret)
	}
}

// ensureNVML() calls NVML Init() and returns an NVML shutdown function and an
// error (nvml.Return). The caller is responsible for calling the shutdown
// function (via defer). This is a noop when DynamicMIG is enabled.
func (l deviceLib) ensureNVML() (func(), nvml.Return) {
	// Long-lived NVML: no init needed, return no-op shutdown func. We could
	// achieve the same by just calling init() once more than shutdown because
	// NVML keeps an internal reference count. I however find it more readable
	// to explicitly initialize NVML only once when DynamicMIG is enabled.
	if featuregates.Enabled(featuregates.DynamicMIG) {
		return func() {}, nvml.SUCCESS
	}

	klog.V(6).Infof("Initializing NVML")
	t0 := time.Now()
	// Its possible there are no GPUs available in NVML.
	// (Eg: All gpus prepared in passthrough-mode)
	// We use the INIT_FLAG_NO_GPUS flag to avoid failing if there are no GPUs.
	ret := l.nvmllib.InitWithFlags(nvml.INIT_FLAG_NO_GPUS)
	if ret != nvml.SUCCESS {
		klog.Warningf("Failed to initialize NVML: %s", ret)
		// Init failed, nothing to cleanup: return no-op.
		return func() {}, ret
	}
	klog.V(6).Infof("t_nvml_init %.3f s", time.Since(t0).Seconds())

	return func() { l.alwaysShutdown() }, nvml.SUCCESS
}

// Discover devices that are allocatable, on this node.
func (l deviceLib) enumerateAllPossibleDevices() (*PerGPUAllocatableDevices, error) {
	perGPUAllocatable, err := l.GetPerGpuAllocatableDevices()
	if err != nil {
		return nil, fmt.Errorf("error enumerating allocatable devices: %w", err)
	}

	if featuregates.Enabled(featuregates.PassthroughSupport) && l.IsVfioEnabled() {
		// Discover passthrough devices and insert them into the
		// `perGPUAllocatable` devices map
		err = l.enumerateGpuVfioDevices(perGPUAllocatable)
		if err != nil {
			return nil, fmt.Errorf("error enumerating GPU PCI devices: %w", err)
		}
	}

	return perGPUAllocatable, nil
}

// GetPerGpuAllocatableDevices() is called once upon startup. It performs device
// discovery, and assembles the set of allocatable devices that will be
// announced by this DRA driver. A list of GPU indices can optionally be
// provided to limit the discovery to a set of physical GPUs.
func (l deviceLib) GetPerGpuAllocatableDevices(indices ...int) (*PerGPUAllocatableDevices, error) {
	klog.Infof("Traverse GPU devices")

	shutdown, ret := l.ensureNVML()
	if ret != nvml.SUCCESS {
		return nil, fmt.Errorf("ensureNVML failed: %w", ret)
	}
	defer shutdown()

	perGPUAllocatable := &PerGPUAllocatableDevices{
		allocatablesMap: make(map[PCIBusID]AllocatableDevices),
	}

	err := l.VisitDevices(func(i int, d nvdev.Device) error {
		if indices != nil && !slices.Contains(indices, i) {
			return nil
		}

		// Prepare data structure for conceptually allocatable devices on this
		// one physical GPU.
		thisGPUAllocatable := make(AllocatableDevices)

		gpuInfo, err := l.getGpuInfo(i, d)
		if err != nil {
			return fmt.Errorf("error getting info for GPU %v: %w", i, err)
		}

		parentdev := &AllocatableDevice{
			Gpu: gpuInfo,
		}

		// Store gpuInfo object for later re-use (lookup by UUID).
		l.gpuInfosByUUID[gpuInfo.UUID] = gpuInfo
		l.gpuUUIDbyPCIBusID[gpuInfo.pciBusID] = gpuInfo.UUID

		if featuregates.Enabled(featuregates.DynamicMIG) {
			dynamicMIGCapable, err := isDynamicMIGCapable(gpuInfo, d)
			if err != nil {
				return fmt.Errorf("error determining DynamicMIG support for GPU %v: %w", i, err)
			}

			// gpuInfo.migEnabled is captured once during enumeration and is
			// never re-read. The branches below rely on this snapshot:
			// toggling MIG mode on Ampere requires a GPU reset that this
			// plugin does not initiate, so within a single plugin lifetime
			// the value cannot change underneath us. A future refactor that
			// moves the migEnabled read past enumeration (or starts
			// re-reading it) must reconsider these branches.
			if dynamicMIGCapable {
				// Best-effort handle cache warmup: store mapping between full-GPU
				// UUID and NVML device handle in a map. Ignore failures.
				if _, ret := l.DeviceGetHandleByUUID(gpuInfo.UUID); ret != nvml.SUCCESS {
					klog.Warningf("DeviceGetHandleByUUID failed: %s", ret)
				}

				// For this full device, inspect all MIG profiles and their possible
				// placements. Side effect: this enriches `gpuInfo` with additional
				// properties (such as the memory slice count, and the maximum
				// capacities as reported by individual MIG profiles).
				migspecs, err := l.inspectMigProfilesAndPlacements(gpuInfo, d)
				if err != nil {
					return fmt.Errorf("error getting MIG info for GPU %v: %w", i, err)
				}

				// When migEnabled is true, parentdev is intentionally NOT added
				// to thisGPUAllocatable — the full GPU is not allocatable on
				// Ampere when MIG cannot be toggled without a GPU reset. The
				// per-GPU shared CounterSet that MIG partitions reference by
				// name is still emitted by the ResourceSlice generators in
				// driver.go, which derive the parent GpuInfo from
				// MigDynamic.Parent when no full-GPU entry exists.

				// We're inside the dynamicMIGCapable=true branch, which already excludes
				// vGPUs masquerading as full GPUs, so supportsMIGModeToggle(d) here precisely means
				// "non-vGPU Hopper+".
				if !gpuInfo.migEnabled || supportsMIGModeToggle(d) {
					thisGPUAllocatable[gpuInfo.CanonicalName()] = parentdev
				}
				for _, migspec := range migspecs {
					dev := &AllocatableDevice{
						MigDynamic: migspec,
					}
					thisGPUAllocatable[migspec.CanonicalName()] = dev
				}

				err = perGPUAllocatable.AddGPUAllocatables(gpuInfo.pciBusID, thisGPUAllocatable)
				if err != nil {
					return fmt.Errorf("error adding allocatables for PCI bus ID %q: %w", gpuInfo.pciBusID, err)
				}

				// Terminate this function -- this is mutually exclusive with static MIG and vfio/passthrough.
				return nil
			}
		}

		migdevs, err := l.discoverMigDevicesByGPU(gpuInfo)
		if err != nil {
			return fmt.Errorf("error discovering MIG devices for GPU %q: %w", gpuInfo.CanonicalName(), err)
		}

		if featuregates.Enabled(featuregates.PassthroughSupport) && l.IsVfioEnabled() {
			// Only if no MIG devices are found, allow VFIO devices.
			klog.Infof("PassthroughSupport enabled, and %d MIG devices found", len(migdevs))
			gpuInfo.vfioEnabled = len(migdevs) == 0
		}

		if !gpuInfo.migEnabled {
			klog.Infof("Adding device %s to allocatable devices", gpuInfo.CanonicalName())
			// No static MIG devices prepared for this physical GPU. Announce
			// physical GPU to be allocatable, and terminate discovery for this
			// phyical GPU.
			thisGPUAllocatable[gpuInfo.CanonicalName()] = parentdev
			err = perGPUAllocatable.AddGPUAllocatables(gpuInfo.pciBusID, thisGPUAllocatable)
			if err != nil {
				return fmt.Errorf("error adding allocatables for PCI bus ID %q: %w", gpuInfo.pciBusID, err)
			}
			return nil
		}

		// Process statically pre-configured MIG devices.
		for _, mdev := range migdevs {
			klog.Infof("Adding MIG device %s to allocatable devices (parent: %s)", mdev.CanonicalName(), gpuInfo.CanonicalName())
			thisGPUAllocatable[mdev.CanonicalName()] = mdev
		}

		// Likely unintentionally stranded capacity (misconfiguration).
		if len(migdevs) == 0 {
			klog.Warningf("Physical GPU %s has MIG mode enabled but no configured MIG devices", gpuInfo.CanonicalName())
		}

		err = perGPUAllocatable.AddGPUAllocatables(gpuInfo.pciBusID, thisGPUAllocatable)
		if err != nil {
			return fmt.Errorf("error adding allocatables for PCI bus ID %s: %w", gpuInfo.pciBusID, err)
		}

		return nil
	})

	if err != nil {
		return nil, fmt.Errorf("error visiting devices: %w", err)
	}

	return perGPUAllocatable, nil
}

func (l deviceLib) discoverMigDevicesByGPU(gpuInfo *GpuInfo) ([]*AllocatableDevice, error) {
	var devices []*AllocatableDevice
	migs, err := l.getMigDevices(gpuInfo)
	if err != nil {
		return nil, fmt.Errorf("error getting MIG devices for GPU %q: %w", gpuInfo.CanonicalName(), err)
	}

	for _, migDeviceInfo := range migs {
		mig := &AllocatableDevice{
			MigStatic: migDeviceInfo,
		}
		devices = append(devices, mig)
	}
	return devices, nil
}

// TODO: Need go-nvlib util for this.
func (l deviceLib) discoverGPUByPCIBusID(pciBusID string) (*AllocatableDevice, []*AllocatableDevice, error) {
	if err := l.Init(); err != nil {
		return nil, nil, err
	}
	defer l.alwaysShutdown()

	var gpu *AllocatableDevice
	var migs []*AllocatableDevice
	err := l.VisitDevices(func(i int, d nvdev.Device) error {
		gpuPCIBusID, err := d.GetPCIBusID()
		if err != nil {
			return fmt.Errorf("error getting PCI bus ID for device %d: %w", i, err)
		}
		if gpuPCIBusID != pciBusID {
			return nil
		}
		gpuInfo, err := l.getGpuInfo(i, d)
		if err != nil {
			return fmt.Errorf("error getting info for GPU %d: %w", i, err)
		}
		migs, err = l.discoverMigDevicesByGPU(gpuInfo)
		if err != nil {
			return fmt.Errorf("error discovering MIG devices for GPU %q: %w", gpuInfo.CanonicalName(), err)
		}
		// If no MIG devices are found, allow VFIO devices.
		gpuInfo.vfioEnabled = len(migs) == 0
		gpu = &AllocatableDevice{
			Gpu: gpuInfo,
		}
		return nil
	})
	if err != nil {
		return nil, nil, fmt.Errorf("error visiting devices: %w", err)
	}
	return gpu, migs, nil
}

// TODO: Need go-nvlib util for this.
func (l deviceLib) discoverVfioDevice(gpuInfo *GpuInfo) (*AllocatableDevice, error) {
	gpus, err := l.nvpci.GetGPUs()
	if err != nil {
		return nil, fmt.Errorf("error getting GPU PCI devices: %w", err)
	}
	for idx, gpu := range gpus {
		if gpu.Address != gpuInfo.pciBusID {
			continue
		}
		vfioDeviceInfo, err := l.getVfioDeviceInfo(idx, gpu)
		if err != nil {
			return nil, fmt.Errorf("error getting VFIO device info: %w", err)
		}
		vfioDeviceInfo.parent = gpuInfo
		return &AllocatableDevice{
			Vfio: vfioDeviceInfo,
		}, nil
	}
	return nil, fmt.Errorf("error discovering VFIO device by PCI bus ID: %s", gpuInfo.pciBusID)
}

// Tear down any MIG devices that are present and don't belong to completed
// claims. This can be improved for tearing down partial state (GI without CI,
// for example).
func (l deviceLib) obliterateStaleMIGDevices(expectedDeviceNames []DeviceName) error {
	err := l.VisitDevices(func(i int, d nvdev.Device) error {
		ginfo, err := l.getGpuInfo(i, d)
		if err != nil {
			return fmt.Errorf("error getting info for GPU %d: %w", i, err)
		}

		migs, err := l.getMigDevices(ginfo)
		if err != nil {
			return fmt.Errorf("error getting MIG devices for GPU %d: %w", i, err)
		}

		for _, mdi := range migs {
			name := mdi.CanonicalName()
			expected := slices.Contains(expectedDeviceNames, name)
			if !expected {
				klog.Warningf("Found unexpected MIG device (%s), attempt to tear down", name)
				if err := l.deleteMigDevice(mdi.LiveTuple()); err != nil {
					return fmt.Errorf("could not delete unexpected MIG device (%s): %w", name, err)
				}
			}
		}

		// If no MIG device was found on this GPU, MIG mode might still be
		// enabled. Disable it in this case.
		if err := l.maybeDisableMigMode(ginfo.UUID, d); err != nil {
			return fmt.Errorf("failed to disable MIG mode for %s (maybeDisableMigMode): %w", ginfo.UUID, err)
		}
		return nil
	})

	if err != nil {
		return fmt.Errorf("error visiting devices: %w", err)
	}
	return nil
}

func (l deviceLib) getGpuInfo(index int, device nvdev.Device) (*GpuInfo, error) {
	minor, ret := device.GetMinorNumber()
	if ret != nvml.SUCCESS {
		return nil, fmt.Errorf("error getting minor number for device %d: %w", index, ret)
	}
	uuid, ret := device.GetUUID()
	if ret != nvml.SUCCESS {
		return nil, fmt.Errorf("error getting UUID for device %d: %w", index, ret)
	}

	migCapable, err := device.IsMigCapable()
	if err != nil {
		return nil, fmt.Errorf("error checking MIG capability for device %d: %w", index, err)
	}

	migEnabled, err := device.IsMigEnabled()
	if err != nil {
		return nil, fmt.Errorf("error checking if MIG mode enabled for device %d: %w", index, err)
	}

	var memoryBytes *uint64
	memory, ret := device.GetMemoryInfo()
	switch ret {
	case nvml.SUCCESS:
		memoryBytes = &(memory.Total)
	case nvml.ERROR_NOT_SUPPORTED:
		memoryBytes = nil
		klog.Warningf("ignoring %v for getting memory info for device %d", ret, index)
	default:
		return nil, fmt.Errorf("error getting memory info for device %d: %w", index, ret)

	}

	productName, ret := device.GetName()
	if ret != nvml.SUCCESS {
		return nil, fmt.Errorf("error getting product name for device %d: %w", index, ret)
	}
	architecture, err := device.GetArchitectureAsString()
	if err != nil {
		return nil, fmt.Errorf("error getting architecture for device %d: %w", index, err)
	}
	brand, err := device.GetBrandAsString()
	if err != nil {
		return nil, fmt.Errorf("error getting brand for device %d: %w", index, err)
	}
	cudaComputeCapability, err := device.GetCudaComputeCapabilityAsString()
	if err != nil {
		return nil, fmt.Errorf("error getting CUDA compute capability for device %d: %w", index, err)
	}
	driverVersion, ret := l.nvmllib.SystemGetDriverVersion()
	if ret != nvml.SUCCESS {
		return nil, fmt.Errorf("error getting driver version: %w", ret)
	}
	cudaDriverVersion, ret := l.nvmllib.SystemGetCudaDriverVersion()
	if ret != nvml.SUCCESS {
		return nil, fmt.Errorf("error getting CUDA driver version: %w", ret)
	}
	pciBusID, err := device.GetPCIBusID()
	if err != nil {
		return nil, fmt.Errorf("error getting PCI bus ID for device %d: %w", index, err)
	}

	// Get the memory-addressing mode supported by the device.
	// On coherent-memory systems, the possible modes are:
	//   - HMM  (Hardware Memory Management)
	//   - ATS  (Address Translation Service)
	//   - None (Supported by the platform but currently inactive)
	//   - ""   (Not supported by the platform)
	var addressingMode *string
	if mode, err := device.GetAddressingModeAsString(); err != nil {
		return nil, fmt.Errorf("error getting addressing mode for device %d: %w", index, err)
	} else if mode != "" {
		addressingMode = &mode
	}

	var pciBusIDAttr *deviceattribute.DeviceAttribute
	attr, err := deviceattribute.GetPCIBusIDAttribute(pciBusID)
	if err != nil {
		return nil, fmt.Errorf("error getting PCI bus ID for device %d: %w", index, err)
	}
	pciBusIDAttr = &attr

	var pcieRootAttr *deviceattribute.DeviceAttribute
	var machineModifiers []deviceattribute.MachineModifier
	if l.sysfsRoot != "" {
		machineModifiers = append(machineModifiers, deviceattribute.WithFSFromRoot(l.sysfsRoot))
	}
	if attr, err := deviceattribute.GetPCIeRootAttributeByPCIBusID(pciBusID, machineModifiers...); err == nil {
		pcieRootAttr = &attr
	} else {
		klog.Warningf("error getting PCIe root for device %d, continuing without attribute: %v", index, err)
	}

	numaNodeAttr := discoverNUMANodeAttribute(pciBusID)

	var migProfiles []*MigProfileInfo
	for i := 0; i < nvml.GPU_INSTANCE_PROFILE_COUNT; i++ {
		giProfileInfo, ret := device.GetGpuInstanceProfileInfo(i)
		if ret == nvml.ERROR_NOT_SUPPORTED {
			continue
		}
		if ret == nvml.ERROR_INVALID_ARGUMENT {
			continue
		}
		if ret != nvml.SUCCESS {
			return nil, fmt.Errorf("error retrieving GpuInstanceProfileInfo for profile %d on GPU %v: %w", i, uuid, ret)
		}

		giPossiblePlacements, ret := device.GetGpuInstancePossiblePlacements(&giProfileInfo)
		if ret == nvml.ERROR_NOT_SUPPORTED {
			continue
		}
		if ret == nvml.ERROR_INVALID_ARGUMENT {
			continue
		}
		if ret != nvml.SUCCESS {
			return nil, fmt.Errorf("error retrieving GpuInstancePossiblePlacements for profile %d on GPU %v: %w", i, uuid, ret)
		}

		var migDevicePlacements []*MigDevicePlacement
		for _, p := range giPossiblePlacements {
			mdp := &MigDevicePlacement{
				GpuInstancePlacement: p,
			}
			migDevicePlacements = append(migDevicePlacements, mdp)
		}

		for j := 0; j < nvml.COMPUTE_INSTANCE_PROFILE_COUNT; j++ {
			for k := 0; k < nvml.COMPUTE_INSTANCE_ENGINE_PROFILE_COUNT; k++ {
				migProfile, err := l.NewMigProfile(i, j, k, giProfileInfo.MemorySizeMB, memory.Total)
				if err != nil {
					return nil, fmt.Errorf("error building MIG profile from GpuInstanceProfileInfo for profile %d on GPU %v: %w", i, uuid, err)
				}

				if migProfile.GetInfo().G != migProfile.GetInfo().C {
					continue
				}

				profileInfo := &MigProfileInfo{
					profile:    migProfile,
					placements: migDevicePlacements,
				}

				migProfiles = append(migProfiles, profileInfo)
			}
		}
	}

	gpuInfo := &GpuInfo{
		UUID:                  uuid,
		minor:                 minor,
		migCapable:            migCapable,
		migEnabled:            migEnabled,
		memoryBytes:           memoryBytes,
		productName:           productName,
		brand:                 brand,
		architecture:          architecture,
		cudaComputeCapability: cudaComputeCapability,
		driverVersion:         driverVersion,
		cudaDriverVersion:     fmt.Sprintf("%v.%v", cudaDriverVersion/1000, (cudaDriverVersion%1000)/10),
		pciBusID:              pciBusID,
		pciBusIDAttr:          pciBusIDAttr,
		pcieRootAttr:          pcieRootAttr,
		numaNodeAttr:          numaNodeAttr,
		migProfiles:           migProfiles,
		addressingMode:        addressingMode,
	}

	if featuregates.Enabled(featuregates.FabricManagerPartitioning) {
		if moduleID, ret := device.GetModuleId(); ret == nvml.SUCCESS {
			gpuInfo.gpuModuleID = moduleID
		} else {
			klog.V(4).Infof("GPU %s: could not resolve gpuModuleID from NVML: %v", gpuInfo.CanonicalName(), ret)
		}
	}

	return gpuInfo, nil
}

func (l deviceLib) enumerateGpuVfioDevices(perGPUAllocatable *PerGPUAllocatableDevices) error {
	// Discover PCI devices.
	gpuPciDevices, err := l.nvpci.GetGPUs()
	if err != nil {
		return fmt.Errorf("error getting GPU PCI devices: %w", err)
	}

	// For each discovered PCI device, look up the corresponding full-GPU-device
	// and construct a corresponding new `AllocatableDevice` object.
	for idx, pci := range gpuPciDevices {
		klog.Infof("Adding VFIO device for discovered GPU PCI device: %s", pci.Address)

		parent := perGPUAllocatable.GetGPUDeviceByPCIBusID(pci.Address)
		if parent != nil && !parent.Gpu.vfioEnabled {
			klog.Infof("Skipping VFIO device for discovered GPU PCI device: %s, vfio is not enabled", pci.Address)
			continue
		}

		vfioDeviceInfo, err := l.getVfioDeviceInfo(idx, pci)
		if err != nil {
			return fmt.Errorf("error getting GPU info from PCI device: %w", err)
		}

		if parent != nil {
			vfioDeviceInfo.parent = parent.Gpu
		} else {
			// Its likely that the parent is nil because the GPU is prepared in passthrough mode.
			klog.Warningf("Skipping association with parent GPU device for VFIO device: %s", pci.Address)
		}

		allocatableDevice := &AllocatableDevice{
			Vfio: vfioDeviceInfo,
		}
		err = perGPUAllocatable.AddAllocatableDevice(allocatableDevice)
		if err != nil {
			return fmt.Errorf("error adding GPU VFIO device: %w", err)
		}
	}

	return nil
}

func (l deviceLib) getVfioDeviceInfo(idx int, device *nvpci.NvidiaPCIDevice) (*VfioDeviceInfo, error) {
	iommuFDEnabled, err := checkIommuFDEnabled(l.hostRoot)
	if err != nil {
		return nil, fmt.Errorf("error checking if IOMMUFD is supported: %w", err)
	}

	var pciBusIDAttr *deviceattribute.DeviceAttribute
	attr, err := deviceattribute.GetPCIBusIDAttribute(device.Address)
	if err != nil {
		return nil, fmt.Errorf("error getting PCI bus ID for device %q: %w", device.Address, err)
	}
	pciBusIDAttr = &attr

	var pcieRootAttr *deviceattribute.DeviceAttribute
	if attr, err := deviceattribute.GetPCIeRootAttributeByPCIBusID(device.Address); err == nil {
		pcieRootAttr = &attr
	} else {
		klog.Warningf("error getting PCIe root for device %q, continuing without attribute: %v", device.Address, err)
	}

	numaNodeAttr := discoverNUMANodeAttribute(device.Address)

	_, memoryBytes := device.Resources.GetTotalAddressableMemory(true)

	vfioModule, err := l.nvpasst.FindBestVFIOVariant(device.Address)
	if err != nil {
		return nil, fmt.Errorf("error finding best VFIO driver for device %q: %w", device.Address, err)
	}

	// Generate a unique UUID for the VFIO device based on the PCI bus ID.
	// This will always map to the same PCI bus ID.
	deviceUUID := uuid.NewSHA1(uuid.NameSpaceDNS, []byte(device.Address)).String()
	vfioDeviceInfo := &VfioDeviceInfo{
		UUID:                   deviceUUID,
		index:                  idx,
		productName:            device.DeviceName,
		PciBusID:               device.Address,
		pciBusIDAttr:           pciBusIDAttr,
		pcieRootAttr:           pcieRootAttr,
		deviceID:               fmt.Sprintf("0x%04x", device.Device),
		vendorID:               fmt.Sprintf("0x%04x", device.Vendor),
		numaNodeAttr:           numaNodeAttr,
		iommuGroup:             device.IommuGroup,
		iommuFDEnabled:         iommuFDEnabled,
		addressableMemoryBytes: memoryBytes,
		vfioModule:             vfioModule,
	}

	return vfioDeviceInfo, nil
}

func (l deviceLib) getMigDevices(gpuInfo *GpuInfo) (map[string]*MigDeviceInfo, error) {
	if !gpuInfo.migEnabled {
		return nil, nil
	}

	shutdown, ret := l.ensureNVML()
	if ret != nvml.SUCCESS {
		return nil, fmt.Errorf("ensureNVML failed: %w", ret)
	}
	defer shutdown()

	device, ret := l.DeviceGetHandleByUUID(gpuInfo.UUID)
	if ret != nvml.SUCCESS {
		return nil, fmt.Errorf("error getting GPU device handle: %w", ret)
	}

	infos := make(map[string]*MigDeviceInfo)
	err := walkMigDevices(device, func(i int, migDevice nvml.Device) error {
		giID, ret := migDevice.GetGpuInstanceId()
		if ret != nvml.SUCCESS {
			return fmt.Errorf("error getting GPU instance ID for MIG device: %w", ret)
		}
		gi, ret := device.GetGpuInstanceById(giID)
		if ret != nvml.SUCCESS {
			return fmt.Errorf("error getting GPU instance for '%v': %w", giID, ret)
		}
		giInfo, ret := gi.GetInfo()
		if ret != nvml.SUCCESS {
			return fmt.Errorf("error getting GPU instance info for '%v': %w", giID, ret)
		}
		ciID, ret := migDevice.GetComputeInstanceId()
		if ret != nvml.SUCCESS {
			return fmt.Errorf("error getting Compute instance ID for MIG device: %w", ret)
		}
		ci, ret := gi.GetComputeInstanceById(ciID)
		if ret != nvml.SUCCESS {
			return fmt.Errorf("error getting Compute instance for '%v': %w", ciID, ret)
		}
		ciInfo, ret := ci.GetInfo()
		if ret != nvml.SUCCESS {
			return fmt.Errorf("error getting Compute instance info for '%v': %w", ciID, ret)
		}
		uuid, ret := migDevice.GetUUID()
		if ret != nvml.SUCCESS {
			return fmt.Errorf("error getting UUID for MIG device: %w", ret)
		}

		var migProfile *MigProfileInfo
		var giProfileInfo *nvml.GpuInstanceProfileInfo
		var ciProfileInfo *nvml.ComputeInstanceProfileInfo
		for _, profile := range gpuInfo.migProfiles {
			profileInfo := profile.profile.GetInfo()
			gipInfo, ret := device.GetGpuInstanceProfileInfo(profileInfo.GIProfileID)
			if ret != nvml.SUCCESS {
				continue
			}
			if giInfo.ProfileId != gipInfo.Id {
				continue
			}
			cipInfo, ret := gi.GetComputeInstanceProfileInfo(profileInfo.CIProfileID, profileInfo.CIEngProfileID)
			if ret != nvml.SUCCESS {
				continue
			}
			if ciInfo.ProfileId != cipInfo.Id {
				continue
			}
			migProfile = profile
			giProfileInfo = &gipInfo
			ciProfileInfo = &cipInfo
		}
		if migProfile == nil {
			return fmt.Errorf("failed to match MIG profile for device %s (GI profile ID %d, CI profile ID %d)", uuid, giInfo.ProfileId, ciInfo.ProfileId)
		}

		placement := MigDevicePlacement{
			GpuInstancePlacement: giInfo.Placement,
		}

		infos[uuid] = &MigDeviceInfo{
			UUID:           uuid,
			Profile:        migProfile.String(),
			ParentMinor:    gpuInfo.minor,
			ParentUUID:     gpuInfo.UUID,
			CIID:           int(ciInfo.Id),
			GIID:           int(giInfo.Id),
			PlacementStart: int(placement.Start),
			PlacementSize:  int(placement.Size),
			GiProfileID:    int(giProfileInfo.Id),
			parent:         gpuInfo,
			giProfileInfo:  giProfileInfo,
			gIInfo:         &giInfo,
			ciProfileInfo:  ciProfileInfo,
			cIInfo:         &ciInfo,
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("error enumerating MIG devices: %w", err)
	}

	if len(infos) == 0 {
		return nil, nil
	}

	return infos, nil
}

func walkMigDevices(d nvml.Device, f func(i int, d nvml.Device) error) error {
	count, ret := nvml.Device(d).GetMaxMigDeviceCount()
	if ret != nvml.SUCCESS {
		return fmt.Errorf("error getting max MIG device count: %w", ret)
	}

	for i := 0; i < count; i++ {
		device, ret := d.GetMigDeviceHandleByIndex(i)
		if ret == nvml.ERROR_NOT_FOUND {
			continue
		}
		if ret == nvml.ERROR_INVALID_ARGUMENT {
			continue
		}
		if ret != nvml.SUCCESS {
			return fmt.Errorf("error getting MIG device handle at index '%v': %w", i, ret)
		}
		err := f(i, device)
		if err != nil {
			return err
		}
	}
	return nil
}

func (l deviceLib) setTimeSlice(uuids []string, timeSlice int) error {
	for _, uuid := range uuids {
		cmd := exec.Command(
			l.nvidiaSMIPath,
			"compute-policy",
			"-i", uuid,
			"--set-timeslice", fmt.Sprintf("%d", timeSlice))

		// In order for nvidia-smi to run, we need update LD_PRELOAD to include the path to libnvidia-ml.so.1.
		cmd.Env = setOrOverrideEnvvar(os.Environ(), "LD_PRELOAD", prependPathListEnvvar("LD_PRELOAD", l.driverLibraryPath))

		output, err := cmd.CombinedOutput()
		if err != nil {
			klog.Errorf("\n%v", string(output))
			return fmt.Errorf("error running nvidia-smi: %w", err)
		}
	}
	return nil
}

func (l deviceLib) setComputeMode(uuids []string, mode string) error {
	for _, uuid := range uuids {
		cmd := exec.Command(
			l.nvidiaSMIPath,
			"-i", uuid,
			"-c", mode)

		// In order for nvidia-smi to run, we need update LD_PRELOAD to include the path to libnvidia-ml.so.1.
		cmd.Env = setOrOverrideEnvvar(os.Environ(), "LD_PRELOAD", prependPathListEnvvar("LD_PRELOAD", l.driverLibraryPath))

		output, err := cmd.CombinedOutput()
		if err != nil {
			klog.Errorf("\n%v", string(output))
			return fmt.Errorf("error running nvidia-smi: %w", err)
		}
	}
	return nil
}

// Get an NVML device handle for a physical GPU. When not in DynamicMIG mode,
// this currently always calls out to NVML's DeviceGetHandleByUUID(). In
// DynamicMIG mode, this function maintains an NVML handle cache and hence
// guarantees fast lookups. This is meant to only be called for physical, full
// GPUs (not MIG devices).
func (l deviceLib) DeviceGetHandleByUUID(uuid string) (nvml.Device, nvml.Return) {
	shutdown, ret := l.ensureNVML()
	if ret != nvml.SUCCESS {
		return nil, ret
	}
	defer shutdown()

	// For now, only use long-lived NVML with cached handles when DynamicMIG is
	// enabled. In all other cases, do not cache handles (this is something we
	// may want to do in the future).
	if !featuregates.Enabled(featuregates.DynamicMIG) {
		return l.nvmllib.DeviceGetHandleByUUID(uuid)
	}

	dev, exists := l.devhandleByUUID[uuid]
	if exists {
		return dev, nvml.SUCCESS
	}

	klog.V(6).Infof("DeviceGetHandleByUUID called for %s, cache miss", uuid)
	// Note(JP): This call can be slow. Hence, the decision to use long-lived
	// handles (at least for DynamicMIG). In theory here we need a request
	// coalescing strategy (otherwise, cache stampede is a thing in practice: a
	// burst of requests with the same UUID might be incoming in a timeframe
	// much shorter than it takes for the call below to succeed. All cache
	// misses then end up doing this expensive lookup, although it only needs to
	// be performed once). For now, I opt for addressing this by warming up the
	// cache during program startup. Given that the set of full GPUs is static
	// and that we currently have no expiry (but a long-lived map), that will
	// work.
	t0 := time.Now()
	dev, ret = l.nvmllib.DeviceGetHandleByUUID(uuid)
	klog.V(7).Infof("t_device_get_handle_by_uuid %.3f s", time.Since(t0).Seconds())

	if ret != nvml.SUCCESS {
		return nil, ret
	}

	// Populate `devhandleByUUID` for fast lookup.
	l.devhandleByUUID[uuid] = dev
	return dev, ret
}

// Assume long-lived NVML session.
func (l deviceLib) createMigDevice(migspec *MigSpec) (*MigDeviceInfo, error) {
	gpu := migspec.Parent
	profile := migspec.Profile
	placement := &migspec.Placement

	tdhbu0 := time.Now()
	// Without handle caching, I've seen this to take up O(10 s).
	device, ret := l.DeviceGetHandleByUUID(gpu.UUID)
	if ret != nvml.SUCCESS {
		return nil, fmt.Errorf("error getting GPU device handle: %w", ret)
	}
	klog.V(7).Infof("t_prep_create_mig_dev_get_dev_handle %.3f s", time.Since(tdhbu0).Seconds())

	tnd0 := time.Now()
	ndev, err := l.NewDevice(device)
	if err != nil {
		return nil, fmt.Errorf("error instantiating nvml dev: %w", err)
	}
	klog.V(7).Infof("t_prep_create_mig_dev_new_dev %.3f s", time.Since(tnd0).Seconds())

	// nvml GetMigMode distinguishes between current and pending -- not exposed
	// in go-nvlib yet. Maybe that distinction is important here.
	// migModeCurrent, migModePending, err := device.GetMigMode()
	// https://github.com/NVIDIA/go-nvlib/blame/7d260da4747c220a6972ebc83e4eb7116fc9b89a/pkg/nvlib/device/device.go#L225
	tcme0 := time.Now()
	migEnabled, err := ndev.IsMigEnabled()
	if err != nil {
		return nil, fmt.Errorf("error checking if MIG mode enabled for device %s: %w", ndev, err)
	}
	klog.V(7).Infof("t_prep_create_mig_dev_check_mig_enabled %.3f s", time.Since(tcme0).Seconds())

	logpfx := fmt.Sprintf("Create %s", migspec.CanonicalName())

	if !migEnabled {
		klog.V(6).Infof("%s: Attempting to enable MIG mode for to-be parent %s", logpfx, gpu.String())
		// If this is newer than A100 and if device unused: enable MIG.
		tem0 := time.Now()
		ret, activationStatus := device.SetMigMode(nvml.DEVICE_MIG_ENABLE)
		if ret != nvml.SUCCESS {
			// activationStatus would return the appropriate error code upon unsuccessful activation
			klog.Warningf("%s: SetMigMode activationStatus (device %s): %s", logpfx, gpu.String(), activationStatus)
			return nil, fmt.Errorf("error enabling MIG mode for device %s: %w", gpu.String(), ret)
		}
		klog.V(1).Infof("%s: MIG mode now enabled for device %s, t_enable_mig %.3f s", logpfx, gpu.String(), time.Since(tem0).Seconds())
	} else {
		klog.V(6).Infof("%s: MIG mode already enabled for device %s", logpfx, gpu.String())
	}

	profileInfo := profile.GetInfo()

	tcgigi0 := time.Now()
	giProfileInfo, ret := device.GetGpuInstanceProfileInfo(profileInfo.GIProfileID)
	if ret != nvml.SUCCESS {
		return nil, fmt.Errorf("error getting GPU instance profile info for '%v': %w", profile, ret)
	}

	gi, ret := device.CreateGpuInstanceWithPlacement(&giProfileInfo, placement)

	// Ambiguity in the NVML API: when requesting a specific placement that is
	// already occupied, NVML returns NVML_ERROR_INSUFFICIENT_RESOURCES rather
	// than NVML_ERROR_ALREADY_EXISTS. Seemingly, to robustly distinguish
	// "already exists" from "blocked by something else", one cannot rely on the
	// error code alone. One must check the device state. Unrelatedly, if this
	// GPU instance already exists, it is unclear if we can and should safely
	// proceed using it. When we're here, we're not expecting it to exist -- an
	// active destruction should be performed before retrying creation. Hence,
	// for now, just return an error without distinguishing "already exists"
	// from any other type of fault.
	if ret != nvml.SUCCESS {
		return nil, fmt.Errorf("error creating GPU instance for '%s': %w", migspec.CanonicalName(), ret)
	}

	giInfo, ret := gi.GetInfo()
	if ret != nvml.SUCCESS {
		return nil, fmt.Errorf("error getting GPU instance info for '%s': %w", migspec.CanonicalName(), ret)
	}

	ciProfileInfo, ret := gi.GetComputeInstanceProfileInfo(profileInfo.CIProfileID, profileInfo.CIEngProfileID)
	if ret != nvml.SUCCESS {
		return nil, fmt.Errorf("error getting Compute instance profile info for '%v': %w", profile, ret)
	}

	ci, ret := gi.CreateComputeInstance(&ciProfileInfo)
	if ret != nvml.SUCCESS {
		return nil, fmt.Errorf("error creating Compute instance for '%v': %w", profile, ret)
	}

	ciInfo, ret := ci.GetInfo()
	if ret != nvml.SUCCESS {
		return nil, fmt.Errorf("error getting GPU instance info for '%v': %w", profile, ret)
	}
	klog.V(6).Infof("t_prep_create_mig_dev_cigi %.3f s", time.Since(tcgigi0).Seconds())

	// Note(JP): for obtaining the UUID of the just-created MIG device, some
	// algorithms walk through all MIG devices on the parent GPU to identify the
	// one that matches the CIID and GIID of the MIG device that was just
	// created. While that is correct, I measured that the time spent in NVML
	// API calls for 'walking all MIG devices' under load under can easily be
	// O(10 s). The UUID can also be obtained by first getting the MIG device
	// handle from the CI and then calling GetUUID() on that handle. A MIG
	// device handle maps 1:1 to a CI in NVML, so once the CI is known, the MIG
	// device handle and its UUID can be retrieved directly without scanning
	// through indices.
	uuid, ret := ciInfo.Device.GetUUID()
	if ret != nvml.SUCCESS {
		return nil, fmt.Errorf("error getting UUID from CI info/device for CI %d: %w", ciInfo.Id, ret)
	}

	// This now probably needs consolidation with the new types MigLiveTuple and
	// MigSpecTuple. Things get confusing.
	migDevInfo := &MigDeviceInfo{
		UUID:           uuid,
		CIID:           int(ciInfo.Id),
		GIID:           int(giInfo.Id),
		ParentMinor:    gpu.minor,
		ParentUUID:     gpu.UUID,
		Profile:        profile.String(),
		PlacementStart: int(placement.Start),
		PlacementSize:  int(placement.Size),
		GiProfileID:    int(giProfileInfo.Id),
		gIInfo:         &giInfo,
		cIInfo:         &ciInfo,
		parent:         gpu,
	}

	klog.V(6).Infof("%s: MIG device created on %s: %+v", logpfx, gpu.String(), migDevInfo.LiveTuple())
	return migDevInfo, nil
}

// Assume long-lived NVML session.
func (l deviceLib) deleteMigDevice(miglt *MigLiveTuple) error {
	parentUUID := miglt.ParentUUID
	giId := miglt.GIID
	ciId := miglt.CIID

	t0 := time.Now()
	migStr := fmt.Sprintf("MIG(parent: %s, %+v)", parentUUID, miglt)
	klog.V(6).Infof("Delete %s", migStr)

	parentNvmlDev, ret := l.DeviceGetHandleByUUID(parentUUID)
	if ret != nvml.SUCCESS {
		return fmt.Errorf("error getting device from UUID '%v': %w", parentUUID, ret)
	}

	// The order of destroying 1) compute instance and 2) GPU instance matters.
	// These resources are hierarchical: compute instances are created inside a
	// GPU instance, so the parent GPU instance cannot be destroyed while
	// children (compute instances) still exist.
	gi, gires := parentNvmlDev.GetGpuInstanceById(giId)

	// Ref docs document this error with "If device doesn't have MIG mode
	// enabled" -- for the unlikely case that we end up in this state (MIG mode
	// was disabled out-of-band?), this should be treated as deletion success.
	if gires == nvml.ERROR_NOT_SUPPORTED {
		klog.Infof("Delete %s: GetGpuInstanceById yielded ERROR_NOT_SUPPORTED: MIG disabled, treat as success", migStr)
		return nil
	}

	// UNINITIALIZED, INVALID_ARGUMENT, NO_PERMISSION
	if gires != nvml.SUCCESS && gires != nvml.ERROR_NOT_FOUND {
		return fmt.Errorf("error getting GPU instance handle for MIG device %s: %w", migStr, gires)
	}

	if gires == nvml.ERROR_NOT_FOUND {
		// In this case assume that no compute instances exist (as of the GI>CI
		// hierarchy) and proceed with attempt-to-disable-MIG-mode
		klog.Infof("Delete %s: GI was not found skip CI cleanup", migStr)
		if err := l.maybeDisableMigMode(parentUUID, parentNvmlDev); err != nil {
			return fmt.Errorf("failed to disable MIG mode for %s (maybeDisableMigMode): %w", parentUUID, err)
		}
		return nil
	}

	// Remainder, with `gi` actually being valid.
	ci, cires := gi.GetComputeInstanceById(ciId)

	// Here we could compare the actual MIG UUID with an expected MIG UUID,
	// to be extra sure that this we want to proceed with deletion.
	// ciInfo, res := ci.GetInfo()
	// if res != nvml.SUCCESS {
	// 	return fmt.Errorf("error calling ci.GetInfo(): %v", ret)
	// }

	// actualMigUUID, res := nvml.DeviceGetUUID(ciInfo.Device)
	// if res != nvml.SUCCESS {
	// 	return fmt.Errorf("nvml.DeviceGetUUID() failed: %v", ret)
	// }
	// if actualMigUUID != expectedMigUUID {
	// 	return fmt.Errorf("UUID mismatch upon deletion: expected: %s actual: %s", expectedMigUUID, actualMigUUID)
	// }

	// Can never be `ERROR_NOT_SUPPORTED` at this point. Can be UNINITIALIZED,
	// INVALID_ARGUMENT, NO_PERMISSION: for those three, it's worth erroring out
	// here (to be retried later).
	if cires != nvml.SUCCESS && cires != nvml.ERROR_NOT_FOUND {
		return fmt.Errorf("error getting Compute instance handle for MIG device %s: %w", migStr, cires)
	}

	// A previous, partial cleanup may actually have already deleted that. Seen
	// in practice. Ignore, and proceed with deleting GPU instance below.
	if cires == nvml.ERROR_NOT_FOUND {
		klog.Infof("Delete %s: CI not found, ignore", migStr)
	} else {
		ret := ci.Destroy()
		if ret != nvml.SUCCESS {
			return fmt.Errorf("error destroying Compute instance: %w", ret)
		}
	}

	// That can for example fail with "In use by another client", in which case
	// we may have performed only a partial cleanup (CI already destroyed; seen
	// in practice).

	// Note that this operation may take O(1 s). In a machine supporting many
	// MIG devices and significant job throughput, this may become noticeable.
	// In a stressing test, I have seen the prep/unprep lock acquisition time
	// out after 10 seconds, when requests pile up.
	ret = gi.Destroy()
	if ret != nvml.SUCCESS {
		return fmt.Errorf("error destroying GPU Instance: %w", ret)
	}
	klog.V(6).Infof("t_delete_mig_device %.3f s", time.Since(t0).Seconds())

	if err := l.maybeDisableMigMode(parentUUID, parentNvmlDev); err != nil {
		return fmt.Errorf("failed to disable MIG mode for %s (maybeDisableMigMode): %w", parentUUID, err)
	}

	return nil
}

func (l deviceLib) maybeDisableMigMode(uuid string, nvmldev nvml.Device) error {
	// Expect the parent GPU to be represented in in `l.gpuInfosByUUID`
	gpu, ok := l.gpuInfosByUUID[uuid]
	if !ok {
		// TODO: this is a programming error -- panic instead
		return fmt.Errorf("uuid not in gpuInfosByUUID: %s", uuid)
	}

	migs, err := l.getMigDevices(gpu)
	if err != nil {
		return fmt.Errorf("error getting MIG devices for %s: %w", gpu.String(), err)
	}

	if len(migs) > 0 {
		klog.V(6).Infof("Leaving MIG mode enabled for device %s (currently present MIG devices: %d)", gpu.String(), len(migs))
		return nil
	}

	// On Ampere/A100, SetMigMode sets a pending mode that requires a GPU reset to activate — leave MIG enabled.
	if !supportsMIGModeToggle(nvmldev) {
		klog.Infof("GPU %s (%s): skipping MIG mode disable (architecture does not support reset-less MIG toggling)",
			gpu.String(), gpu.architecture)
		return nil
	}

	klog.V(6).Infof("Attempting to disable MIG mode for device %s", gpu.String())
	t0 := time.Now()
	ret, activationStatus := nvmldev.SetMigMode(nvml.DEVICE_MIG_DISABLE)
	klog.V(6).Infof("t_disable_mig %.3f s", time.Since(t0).Seconds())
	if ret != nvml.SUCCESS {
		// activationStatus would return the appropriate error code upon unsuccessful activation
		klog.Warningf("SetMigMode activationStatus (device %s): %s", gpu.String(), activationStatus)
		// We could also log this as an error and proceed, and hope for the
		// state machine to clean this up in the future. Probably not a good
		// idea.
		return fmt.Errorf("error disabling MIG mode for device %s: %w", gpu.String(), ret)
	}
	// Note: when we're here, disabling MIG mode might still have failed.
	// `activationStatus` may reflect "in use by another client".
	klog.V(1).Infof("Called nvml.SetMigMode(nvml.DEVICE_MIG_DISABLE) for device %s, got activationStatus: %s", gpu.String(), activationStatus)
	return nil
}

// Returns a flat list of all possible physical MIG configurations for a
// specific GPU. Specifically, this discovers all possible profiles, and then
// then determines the possible placements for each profile.
func (l deviceLib) inspectMigProfilesAndPlacements(gpuInfo *GpuInfo, device nvdev.Device) ([]*MigSpec, error) {
	var infos []*MigSpec

	maxCapacities := make(PartCapacityMap)
	maxMemSlicesConsumed := 0

	err := device.VisitMigProfiles(func(migProfile nvdev.MigProfile) error {
		if migProfile.GetInfo().C != migProfile.GetInfo().G {
			return nil
		}

		if migProfile.GetInfo().CIProfileID == nvml.COMPUTE_INSTANCE_PROFILE_1_SLICE_REV1 {
			return nil
		}

		giProfileInfo, ret := device.GetGpuInstanceProfileInfo(migProfile.GetInfo().GIProfileID)
		if ret == nvml.ERROR_NOT_SUPPORTED {
			return nil
		}
		if ret == nvml.ERROR_INVALID_ARGUMENT {
			return nil
		}
		if ret != nvml.SUCCESS {
			return fmt.Errorf("error getting GI Profile info for MIG profile %v: %w", migProfile, ret)
		}

		giPlacements, ret := device.GetGpuInstancePossiblePlacements(&giProfileInfo)
		if ret == nvml.ERROR_NOT_SUPPORTED {
			return nil
		}
		if ret == nvml.ERROR_INVALID_ARGUMENT {
			return nil
		}
		if ret != nvml.SUCCESS {
			return fmt.Errorf("error getting GI possible placements for MIG profile %v: %w", migProfile, ret)
		}

		for _, giPlacement := range giPlacements {
			mi := &MigSpec{
				Parent:        gpuInfo,
				Profile:       migProfile,
				GIProfileInfo: giProfileInfo,
				Placement:     giPlacement,
			}
			infos = append(infos, mi)

			// Assume that the largest MIG profile consumes all memory slices,
			// and hence we can infer the memory slice count by looking at the
			// Size property of all MigPP objects, and picking the maximum.
			maxMemSlicesConsumed = max(maxMemSlicesConsumed, int(giPlacement.Size))

			// Across all MIG profiles, identify the largest value for each
			// capacity dimension. They probably all corresponding to the same
			// profile.
			caps := mi.Capacities()
			for name, cap := range caps {
				setMax(maxCapacities, name, cap)
			}
		}
		return nil
	})

	klog.V(1).Infof("%s: Per-capacity maximum across all MIG profiles+placements: %v", gpuInfo.String(), maxCapacities)
	klog.V(1).Infof("%s: Largest MIG placement size seen (maxMemSlicesConsumed): %d", gpuInfo.String(), maxMemSlicesConsumed)

	if err != nil {
		return nil, fmt.Errorf("error visiting MIG profiles: %w", err)
	}

	// Mutate the full-device information container `gpuInfo`; enrich it with
	// detail obtained from walking MIG devices. Assume that the largest MIG
	// profile seen consumes all memory slices; equate maxMemSlicesConsumed =
	// memSliceCount.
	gpuInfo.AddDetailAfterWalkingMigProfiles(maxCapacities, maxMemSlicesConsumed)
	return infos, nil
}

// FindMigDevBySpec() tests if a MIG device defined by the provided
// `MigSpecTuple` exists. If it exists, a pointer to a corresponding
// `MigLiveTuple` is returned. If it doesn't exist, a nil pointer is returned.
// if an NVML API call fails along the way, a nil pointer and a non-nil error is
// returned.
func (l deviceLib) FindMigDevBySpec(ms *MigSpecTuple) (*MigLiveTuple, error) {
	parentUUID := l.gpuUUIDbyPCIBusID[ms.ParentPCIBusID]
	parent, ret := l.DeviceGetHandleByUUID(parentUUID)
	if ret != nvml.SUCCESS {
		return nil, fmt.Errorf("error getting device handle by UUID for %s: %w", parentUUID, ret)
	}

	count, _ := parent.GetMaxMigDeviceCount()

	for i := range count {
		migHandle, ret := parent.GetMigDeviceHandleByIndex(i)
		if ret != nvml.SUCCESS {
			klog.Infof("GetMigDeviceHandleByIndex ret not success")
			// Slot empty or invalid: treat as device does not currently exist.
			continue
		}

		giId, ret := migHandle.GetGpuInstanceId()
		if ret != nvml.SUCCESS {
			return nil, fmt.Errorf("failed to get GI ID: %w", ret)
		}

		giHandle, ret := parent.GetGpuInstanceById(giId)
		if ret != nvml.SUCCESS {
			return nil, fmt.Errorf("failed to get GI handle for ID %d: %w", giId, ret)
		}

		giInfo, ret := giHandle.GetInfo()
		if ret != nvml.SUCCESS {
			return nil, fmt.Errorf("failed to get GI info: %w", ret)
		}

		klog.V(7).Infof("FindMigDevBySpec: saw MIG dev with profile id %d and placement start %d", giInfo.ProfileId, giInfo.Placement.Start)

		if int(giInfo.ProfileId) != ms.ProfileID {
			klog.V(7).Infof("profile ID mismatch: looking for %d", ms.ProfileID)
			continue
		}

		if int(giInfo.Placement.Start) != ms.PlacementStart {
			klog.V(7).Infof("placement start mismatch: looking for %d", ms.PlacementStart)
			continue
		}

		klog.V(4).Infof("FindMigDevBySpec: match found for profile ID %d and placement start %d", giInfo.ProfileId, giInfo.Placement.Start)

		// In our way of managing MIG devices, this should always be zero --
		// nevertheless, perform the lookup. If the lookup fails, the MIG device
		// may have been created only partially (only GI, but not CI). In that
		// case, proceed.
		ciId := 0
		ciId, ret = migHandle.GetComputeInstanceId()
		if ret != nvml.SUCCESS {
			klog.V(4).Infof("FindMigDevBySpec(): failed to get CI ID: %v", ret)
		}

		uuid := ""
		uuid, ret = migHandle.GetUUID()
		if ret != nvml.SUCCESS {
			klog.V(4).Infof("FindMigDevBySpec(): failed to get MIG UUID: %v", ret)
		}

		// Found device matching the spec, return handle. For subsequent
		// deletion of a potentially partially prepared MIG device, it is OK if
		// CIID and uuid are zero values.
		mlt := MigLiveTuple{
			ParentMinor: ms.ParentMinor,
			ParentUUID:  parentUUID,
			GIID:        giId,
			CIID:        ciId,
			MigUUID:     uuid,
		}

		klog.Infof("FindMigDevBySpec result: %+v", mlt)
		return &mlt, nil
	}

	klog.Infof("Iterated through all potential MIG devs -- no candidate found")
	return nil, nil
}

// Mutate map `m` in-place: insert into map if the current QualifiedName does
// not yet exist as a key. Otherwise, update item in map if the incoming value
// `v` is larger than the one currently stored in the map.
func setMax(m map[resourceapi.QualifiedName]resourceapi.DeviceCapacity, k resourceapi.QualifiedName, v resourceapi.DeviceCapacity) {
	if cur, ok := m[k]; !ok || v.Value.Value() > cur.Value.Value() {
		m[k] = v
	}
}

// Disable GPU Persistence mode
//
// Currently persistence mode may be toggled in two ways:
// 1. Using `nvidia-smi -pm`
// 2. Using the Legacy NVML API `nvml.SetPersistenceMode`
//
// When using nvidia-smi, there are 2 possible scenarios:
//  1. If nvidia-persistenced is running, then nvidia-smi uses it to toggle
//     persistence mode.
//  2. If nvidia-persistenced is not running, then nvidia-smi falls back to the
//     legacy NVML API.
//
// Since we do not know if nvidia-persistenced is managing the GPU or not, we
// disable persistence mode using nvidia-smi. This will guarantee the operation
// is successful.
//
// Note: we `chroot` into the device root to properly run nvidia-smi.
func (l deviceLib) disableGPUPersistenceMode(pciAddress string) error {
	cmd := exec.Command(
		"chroot",
		l.devRoot,
		"nvidia-smi",
		"-i",
		pciAddress,
		"-pm",
		"0")

	output, err := cmd.CombinedOutput()
	klog.V(4).Infof("disableGPUPersistenceMode command: %s, output: \n%s", cmd.String(), string(output))
	if err != nil {
		return fmt.Errorf("error running nvidia-smi: %w", err)
	}
	return nil
}

// Enable GPU Persistence mode
//
// While persistence mode disablement can be done using nvidia-smi, we cannot
// do the same for enablement. nvidia-persistenced currently does not support
// hotplug of GPUs.
// In our case, it triggers the same issue when all GPUs are in passthrough mode
// and then put back one by one onto the nvidia driver via NodeUnprepareResources.
// `nvidia-smi -i <id> -pm 1` will fail from the second GPU onwards.
//
// Hence, we only support the Legacy GPU persistence mode when the
// `PassthroughSupport` feature gate is enabled and we're switching between the
// nvidia driver and the vfio-pci driver to manage the GPU.
func (l deviceLib) enableGPUPersistenceMode(pciAddress string) error {
	shutdown, ret := l.ensureNVML()
	defer shutdown()
	if ret != nvml.SUCCESS {
		return fmt.Errorf("error ensuring NVML: %w", ret)
	}

	device, ret := l.nvmllib.DeviceGetHandleByPciBusId(pciAddress)
	if ret != nvml.SUCCESS {
		return fmt.Errorf("error getting device handle by PCI bus ID %s: %w", pciAddress, ret)
	}

	// Check if persistence mode is already enabled.
	mode, ret := nvml.DeviceGetPersistenceMode(device)
	if ret != nvml.SUCCESS {
		return fmt.Errorf("error getting persistence mode: %w", ret)
	}
	if mode == nvml.FEATURE_ENABLED {
		klog.Infof("Persistence mode is already enabled for GPU PCI device %s", pciAddress)
		return nil
	}

	ret = device.SetPersistenceMode(nvml.FEATURE_ENABLED)
	if ret != nvml.SUCCESS {
		return fmt.Errorf("error setting persistence mode: %w", ret)
	}
	klog.V(4).Infof("Enabled persistence mode for GPU PCI device %s", pciAddress)

	return nil
}

// For testing on non-Ampere hardware, set the environment variable
// NVIDIA_DRA_TEST_FORCE_GPU_ARCH=ampere to force GPU arch to be Ampere.
// This exercises the prevent-MIG-toggle code paths on any
// MIG-capable GPU, which is otherwise only reachable on A100.
var forceToBeAmpere = strings.EqualFold(strings.TrimSpace(os.Getenv("NVIDIA_DRA_TEST_FORCE_GPU_ARCH")), "ampere")

// supportsMIGModeToggle reports whether the GPU supports toggling MIG mode
// without requiring a GPU reset. Hopper (H100) and newer support reset-less
// MIG mode toggling. Ampere (A100) does not.
func supportsMIGModeToggle(dev nvml.Device) bool {
	if forceToBeAmpere {
		return false
	}
	arch, ret := dev.GetArchitecture()
	if ret != nvml.SUCCESS {
		return false
	}
	return arch >= nvml.DEVICE_ARCH_HOPPER
}

func isDynamicMIGCapable(gpuInfo *GpuInfo, dev nvdev.Device) (bool, error) {
	if !gpuInfo.migCapable {
		klog.Warningf("GPU %s: hardware does not support MIG - skipping DynamicMIG", gpuInfo.String())
		return false, nil
	}

	vMode, vret := dev.GetVirtualizationMode()
	if vret != nvml.SUCCESS {
		return false, fmt.Errorf("error getting GPU virtualization mode: %w", vret)
	}

	// In a vGPU guest the MIG mode and partition size are fixed by the vGPU
	// profile assigned to the VM — a guest cannot dynamically repartition a
	// vGPU. Skip DynamicMIG.
	if vMode == nvml.GPU_VIRTUALIZATION_MODE_VGPU {
		klog.Warningf("GPU %s: vGPU guest detected — skipping DynamicMIG", gpuInfo.String())
		return false, nil
	}

	// Hopper+ supports reset-less MIG mode toggling, so advertise both the full
	// GPU and the dynamic MIG profiles irrespective of current MIG state.
	if supportsMIGModeToggle(dev) {
		return true, nil
	}

	// On Ampere/A100, once MIG is enabled we can still advertise the dynamic MIG
	// profiles, but not the full GPU because disabling MIG would require a GPU reset.
	if gpuInfo.migEnabled {
		klog.Infof("GPU %s: MIG mode is enabled and cannot be toggled without GPU reset", gpuInfo.String())
		return true, nil
	}

	klog.Infof("GPU %s: MIG mode is disabled and cannot be toggled without GPU reset", gpuInfo.String())
	return false, nil
}
