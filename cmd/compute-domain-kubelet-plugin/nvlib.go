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
	"strings"

	"k8s.io/apimachinery/pkg/util/version"
	"k8s.io/klog/v2"
	"k8s.io/mount-utils"

	"github.com/google/uuid"

	nvdev "github.com/NVIDIA/go-nvlib/pkg/nvlib/device"
	"github.com/NVIDIA/go-nvml/pkg/nvml"

	"sigs.k8s.io/dra-driver-nvidia-gpu/internal/common"
	"sigs.k8s.io/dra-driver-nvidia-gpu/internal/lookup/root"
	fm "sigs.k8s.io/dra-driver-nvidia-gpu/pkg/fabricmanager"
	"sigs.k8s.io/dra-driver-nvidia-gpu/pkg/featuregates"
)

const (
	procDriverNvidiaPath             = "/proc/driver/nvidia"
	nvidiaCapsImexChannelsDeviceName = "nvidia-caps-imex-channels"
	nvidiaCapFabricImexMgmtPath      = "/proc/driver/nvidia/capabilities/fabric-imex-mgmt"
)

type deviceLib struct {
	nvdev.Interface
	nvmllib                  nvml.Interface
	driverLibraryPath        string
	devRoot                  string
	nvidiaSMIPath            string
	maxImexChannelCount      int
	nvCapImexChanDevInfos    []*common.NVcapDeviceInfo
	isSingleNodeNVLinkSystem bool
}

func newDeviceLib(driver *root.Driver) (*deviceLib, error) {
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

	d := deviceLib{
		Interface:           nvdev.New(nvmllib),
		nvmllib:             nvmllib,
		driverLibraryPath:   driverLibraryPath,
		devRoot:             driver.DevRoot,
		nvidiaSMIPath:       nvidiaSMIPath,
		maxImexChannelCount: 0,
	}

	mic, err := d.getImexChannelCount()
	if err != nil {
		return nil, fmt.Errorf("error getting max IMEX channel count: %w", err)
	}
	d.maxImexChannelCount = mic

	// Iterate through [0, mic-1] to pre-compute objects for CDI specs
	// (major/minor dev node numbers won't change at runtime).
	for i := range mic {
		info, err := d.getNVCapIMEXChannelDeviceInfo(i)
		if err != nil {
			return nil, fmt.Errorf("error getting nvcap for IMEX channel '%d': %w", i, err)
		}
		d.nvCapImexChanDevInfos = append(d.nvCapImexChanDevInfos, info)
	}

	// Skip procfs unmount when running against mock NVML — there is no
	// real kernel driver, so /proc/driver/nvidia does not exist.
	if !common.UsingAltProcDevices() {
		if err := d.unmountRecursively(procDriverNvidiaPath); err != nil {
			return nil, fmt.Errorf("error recursively unmounting %s: %w", procDriverNvidiaPath, err)
		}
	}

	if featuregates.Enabled(featuregates.FabricManagerPartitioning) {
		isSingleNodeNVLinkSystem, err := d.checkIsSingleNodeNVLinkSystem()
		if err != nil {
			return nil, fmt.Errorf("error checking if node is single-node NVLink system: %w", err)
		}
		d.isSingleNodeNVLinkSystem = isSingleNodeNVLinkSystem
	}
	return &d, nil
}

func (l deviceLib) init() error {
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
	ret := l.nvmllib.Shutdown()
	if ret != nvml.SUCCESS {
		klog.Warningf("error shutting down NVML: %v", ret)
	}
}

func (l deviceLib) getDriverVersion() (*version.Version, error) {
	if err := l.init(); err != nil {
		return nil, fmt.Errorf("error initializing NVML: %w", err)
	}
	defer l.alwaysShutdown()

	driverVersion, ret := l.nvmllib.SystemGetDriverVersion()
	if ret != nvml.SUCCESS {
		return nil, fmt.Errorf("error getting driver version: %w", ret)
	}

	// Parse the version using Kubernetes version utilities
	v, err := version.ParseGeneric(driverVersion)
	if err != nil {
		return nil, fmt.Errorf("invalid driver version format '%s': %w", driverVersion, err)
	}

	return v, nil
}

func (l deviceLib) enumerateAllPossibleDevices(config *Config) (AllocatableDevices, error) {
	alldevices := make(AllocatableDevices)

	computeDomainChannels, err := l.enumerateComputeDomainChannels(config)
	if err != nil {
		return nil, fmt.Errorf("error enumerating ComputeDomain channel devices: %w", err)
	}
	for k, v := range computeDomainChannels {
		alldevices[k] = v
	}

	computeDomainDaemons, err := l.enumerateComputeDomainDaemons(config)
	if err != nil {
		return nil, fmt.Errorf("error enumerating ComputeDomain daemon devices: %w", err)
	}
	for k, v := range computeDomainDaemons {
		alldevices[k] = v
	}

	return alldevices, nil
}

func (l deviceLib) enumerateComputeDomainChannels(config *Config) (AllocatableDevices, error) {
	devices := make(AllocatableDevices)

	for i := range l.maxImexChannelCount {
		computeDomainChannelInfo := &ComputeDomainChannelInfo{
			ID: i,
		}
		deviceInfo := &AllocatableDevice{
			Channel: computeDomainChannelInfo,
		}
		devices[computeDomainChannelInfo.CanonicalName()] = deviceInfo
	}

	return devices, nil
}

func (l deviceLib) enumerateComputeDomainDaemons(config *Config) (AllocatableDevices, error) {
	devices := make(AllocatableDevices)
	computeDomainDaemonInfo := &ComputeDomainDaemonInfo{
		ID: 0,
	}
	deviceInfo := &AllocatableDevice{
		Daemon: computeDomainDaemonInfo,
	}
	devices[computeDomainDaemonInfo.CanonicalName()] = deviceInfo
	return devices, nil
}

func (l deviceLib) getCliqueID() (string, error) {
	if err := l.init(); err != nil {
		return "", fmt.Errorf("error initializing deviceLib: %w", err)
	}
	defer l.alwaysShutdown()

	if featuregates.Enabled(featuregates.CrashOnNVLinkFabricErrors) {
		return l.getCliqueIDStrict()
	}
	return l.getCliqueIDLegacy()
}

// getCliqueIDLegacy uses IsFabricAttached() and falls back gracefully on errors.
func (l deviceLib) getCliqueIDLegacy() (string, error) {
	uniqueClusterUUIDs := make(map[string]struct{})
	uniqueCliqueIDs := make(map[string]struct{})

	err := l.VisitDevices(func(i int, d nvdev.Device) error {
		duid, ret := d.GetUUID()
		if ret != nvml.SUCCESS {
			return fmt.Errorf("failed to read device uuid (%d): %w", i, ret)
		}

		isFabricAttached, err := d.IsFabricAttached()
		if err != nil {
			return fmt.Errorf("error checking if fabric is attached (device %d/%s): %w", i, duid, err)
		}

		if !isFabricAttached {
			klog.V(4).Infof("no-clique fallback: fabric not attached (device %d/%s)", i, duid)
			return nil
		}

		// TODO: explore using GetGpuFabricInfoV() which can return
		// nvmlGpuFabricInfo_v3_t which contains `state`, `status`, and
		// `healthSummary`. The latter we may at least want to log (may be
		// "unhealthy"). See
		// https://docs.nvidia.com/deploy/nvml-api/group__nvmlFabricDefs.html
		info, ret := d.GetGpuFabricInfo()
		if ret != nvml.SUCCESS {
			return fmt.Errorf("failed to get GPU fabric info (device %d/%s): %w", i, duid, ret)
		}

		clusterUUID, err := uuid.FromBytes(info.ClusterUuid[:])
		if err != nil {
			return fmt.Errorf("invalid cluster UUID (device %d/%s): %w", i, duid, err)
		}

		cliqueID := fmt.Sprintf("%d", info.CliqueId)

		uniqueClusterUUIDs[clusterUUID.String()] = struct{}{}
		uniqueCliqueIDs[cliqueID] = struct{}{}
		klog.V(4).Infof("identified fabric clique UUID/ID (device %d/%s): %s/%s", i, duid, clusterUUID.String(), cliqueID)

		return nil
	})
	if err != nil {
		return "", fmt.Errorf("error getting fabric information from one or more devices: %w", err)
	}

	if len(uniqueClusterUUIDs) == 0 && len(uniqueCliqueIDs) == 0 {
		return "", nil
	}

	if len(uniqueClusterUUIDs) != 1 {
		return "", fmt.Errorf("unexpected number of unique ClusterUUIDs found on devices")
	}

	if len(uniqueCliqueIDs) != 1 {
		return "", fmt.Errorf("unexpected number of unique CliqueIDs found on devices")
	}

	for clusterUUID := range uniqueClusterUUIDs {
		for cliqueID := range uniqueCliqueIDs {
			return fmt.Sprintf("%s.%s", clusterUUID, cliqueID), nil
		}
	}

	return "", fmt.Errorf("unexpected return")
}

// getCliqueIDStrict performs strict validation of NVLink fabric state and crashes on errors.
func (l deviceLib) getCliqueIDStrict() (string, error) {
	uniqueClusterUUIDs := make(map[string]struct{})
	uniqueCliqueIDs := make(map[string]struct{})

	err := l.VisitDevices(func(i int, d nvdev.Device) error {
		duid, ret := d.GetUUID()
		if ret != nvml.SUCCESS {
			return fmt.Errorf("failed to read device uuid (%d): %w", i, ret)
		}

		// Check if platform supports NVLink fabric using GetGpuFabricInfo
		// TODO: explore using GetGpuFabricInfoV() which can return
		// nvmlGpuFabricInfo_v3_t which contains `state`, `status`, and
		// `healthSummary`. The latter we may at least want to log (may be
		// "unhealthy"). See
		// https://docs.nvidia.com/deploy/nvml-api/group__nvmlFabricDefs.html
		info, ret := d.GetGpuFabricInfo()
		if ret == nvml.ERROR_NOT_SUPPORTED {
			klog.V(4).Infof("no-clique fallback: NVLink fabric not supported by driver (device %d/%s, error: ERROR_NOT_SUPPORTED)", i, duid)
			return nil
		}

		if ret != nvml.SUCCESS {
			return fmt.Errorf("failed to get GPU fabric info (device %d/%s): %w", i, duid, ret)
		}

		if info.State == nvml.GPU_FABRIC_STATE_NOT_SUPPORTED {
			klog.V(4).Infof("no-clique fallback: NVLink fabric not supported by device (device %d/%s, error: GPU_FABRIC_STATE_NOT_SUPPORTED)", i, duid)
			return nil
		}

		// NVLink fabric is supported - check if registration completed
		if info.State != nvml.GPU_FABRIC_STATE_COMPLETED {
			// Ignore pending NVLink registration on single-node NVLink systems.
			// In FabricManager multi-tenancy mode, the GPUs may not be registered until a
			// partition they belong to is activated. We are blanket ignoring this state as
			// we can't yet differentiate between real NVLink failures and this scenario.
			if featuregates.Enabled(featuregates.FabricManagerPartitioning) && l.isSingleNodeNVLinkSystem {
				klog.V(4).Infof("no-clique fallback: ignoring incomplete NVLink registration (device=%d/%s, state=%d) on single-node NVLink system", i, duid, info.State)
				return nil
			}
			return fmt.Errorf("NVLink fabric not attached (device %d/%s): state=%d, refusing to start", i, duid, info.State)
		}

		// Registration completed - check Status field for errors (only valid when State == COMPLETED)
		if nvml.Return(info.Status) != nvml.SUCCESS {
			return fmt.Errorf("NVLink fabric registration error (device %d/%s): status=%v, refusing to start", i, duid, nvml.Return(info.Status))
		}

		// Cluster UUID with zero value: treat as MNNVL not supported. Expected
		// for systems which are NVLink-capable, but not MNNVL-capable.
		if info.ClusterUuid == [16]uint8{} {
			klog.V(4).Infof("no-clique fallback: cluster UUID is zero, treat as fabric not attached (device %d/%s)", i, duid)
			return nil
		}

		klog.V(6).Infof("NVLink fabric attached (device %d/%s)", i, duid)

		clusterUUID, err := uuid.FromBytes(info.ClusterUuid[:])
		if err != nil {
			return fmt.Errorf("invalid cluster UUID (device %d/%s): %w", i, duid, err)
		}

		cliqueID := fmt.Sprintf("%d", info.CliqueId)

		uniqueClusterUUIDs[clusterUUID.String()] = struct{}{}
		uniqueCliqueIDs[cliqueID] = struct{}{}
		klog.V(4).Infof("identified fabric clique UUID/ID (device %d/%s): %s/%s", i, duid, clusterUUID.String(), cliqueID)

		return nil
	})
	if err != nil {
		return "", fmt.Errorf("error getting fabric information from one or more devices: %w", err)
	}

	if len(uniqueClusterUUIDs) == 0 && len(uniqueCliqueIDs) == 0 {
		return "", nil
	}

	if len(uniqueClusterUUIDs) != 1 {
		return "", fmt.Errorf("unexpected number of unique ClusterUUIDs found on devices")
	}

	if len(uniqueCliqueIDs) != 1 {
		return "", fmt.Errorf("unexpected number of unique CliqueIDs found on devices")
	}

	for clusterUUID := range uniqueClusterUUIDs {
		for cliqueID := range uniqueCliqueIDs {
			return fmt.Sprintf("%s.%s", clusterUUID, cliqueID), nil
		}
	}

	return "", fmt.Errorf("unexpected return")
}

func (l deviceLib) getImexChannelCount() (int, error) {
	// TODO: Pull this value from /proc/driver/nvidia/params
	return 2048, nil
}

func (l deviceLib) getNVCapIMEXChannelDeviceInfo(channelID int) (*common.NVcapDeviceInfo, error) {
	major, err := common.GetDeviceMajor(nvidiaCapsImexChannelsDeviceName)
	if err != nil {
		return nil, fmt.Errorf("error getting device major: %w", err)
	}

	info := &common.NVcapDeviceInfo{
		Major:  major,
		Minor:  channelID,
		Mode:   0666,
		Modify: 0,
		Path:   fmt.Sprintf("/dev/nvidia-caps-imex-channels/channel%d", channelID),
	}

	return info, nil
}

// nvidiaImexCtlBinaryName is the CLI tool shipped alongside nvidia-imex that
// queries the local IMEX daemon's readiness.
const nvidiaImexCtlBinaryName = "nvidia-imex-ctl"

// checkHostIMEXReady validates that the operator-managed host nvidia-imex
// daemon (see the HostManagedIMEXDaemon feature gate) is up and ready.
//
// It runs `nvidia-imex-ctl -q --u=<socketPath>` ("Query the status of the
// IMEX daemon once and return", connecting over the given Unix domain
// socket) chrooted into the driver root.
//
// This check requires the host's nvidia-imex daemon to be configured with
// IMEX_CMD_UNIX_DOMAIN_PATH (see site/content/docs/prerequisites.md).
func (l deviceLib) checkHostIMEXReady(socketPath string) error {
	driver := root.New(root.WithDriverRoot(l.devRoot))
	if _, err := driver.BinaryPath(nvidiaImexCtlBinaryName); err != nil {
		return fmt.Errorf("%s not found under driver root %q: %w (is nvidia-imex installed on this node?)",
			nvidiaImexCtlBinaryName, l.devRoot, err)
	}

	cmd := exec.Command("chroot", l.devRoot, nvidiaImexCtlBinaryName, "-q", "--u="+socketPath) //nolint:gosec
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("error running %s (socket %q): %w, output: %s", nvidiaImexCtlBinaryName, socketPath, err, string(output))
	}

	if !strings.Contains(string(output), "READY") {
		return fmt.Errorf("host nvidia-imex daemon not ready: %s", string(output))
	}

	return nil
}

func (l deviceLib) unmountRecursively(root string) error {
	// Get a reference to the mount executable.
	mountExecutable, err := exec.LookPath("mount")
	if err != nil {
		return fmt.Errorf("error looking up mount executable: %w", err)
	}
	mounter := mount.New(mountExecutable)

	// Build a recursive helper function to unmount depth-first.
	var helper func(path string) error
	helper = func(path string) error {
		// Read the directory contents of path.
		entries, err := os.ReadDir(path)
		if err != nil {
			return fmt.Errorf("failed to read directory %s: %w", path, err)
		}

		// Process each entry, recursively.
		for _, entry := range entries {
			subPath := filepath.Join(path, entry.Name())
			if entry.IsDir() {
				if err := helper(subPath); err != nil {
					return err
				}
			}
		}

		// After processing all children, unmount the current directory if it's a mount point.
		mounted, err := mounter.IsMountPoint(path)
		if err != nil {
			return fmt.Errorf("failed to check mount point %s: %w", path, err)
		}
		if mounted {
			if err := mounter.Unmount(path); err != nil {
				return fmt.Errorf("failed to unmount %s: %w", path, err)
			}
		}

		return nil
	}

	return helper(root)
}

// isSingleNodeNVLinkSystem returns true if the node is a single-node NVLink
// system with one or more NVSwitch (or newer NVLink5 switch) devices.
func (l deviceLib) checkIsSingleNodeNVLinkSystem() (bool, error) {
	hasLocalFabric, err := fm.HasFabricManagerFabric(l.devRoot)
	if err != nil {
		return false, fmt.Errorf("failed to check if node has local fabric: %w", err)
	}

	if !hasLocalFabric {
		return false, nil
	}

	return true, nil
}
