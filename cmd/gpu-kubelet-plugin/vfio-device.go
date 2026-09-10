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
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"k8s.io/klog/v2"
)

const (
	kernelIommuGroupPath         = "/sys/kernel/iommu_groups"
	nvidiaDriver                 = "nvidia"
	sysModulePath                = "/sys/module"
	pciDevicesPath               = "/sys/bus/pci/devices"
	pciDriversPath               = "/sys/bus/pci/drivers"
	vfioDevicesRoot              = "/dev/vfio"
	vfioDevicesPath              = "/dev/vfio/devices"
	iommuDevicePath              = "/dev/iommu"
	nvidiaPersistencedSocketPath = "/run/nvidia-persistenced/socket"
	gpuFreeCheckInterval         = 1 * time.Second
	gpuFreeCheckTimeout          = 60 * time.Second
	// Keep driverChangeTimeout short to avoid blocking the plugin process
	// for too long from serving other device preparation/unpreparation
	// requests.
	driverChangeTimeout = 15 * time.Second
)

type VfioPciManager struct {
	sync.Mutex
	containerDriverRoot string
	hostDriverRoot      string
	nvlib               *deviceLib
	nvidiaEnabled       bool

	inflightDriverSwitches map[string]struct{}
}

func NewVfioPciManager(containerDriverRoot string, hostDriverRoot string, nvlib *deviceLib, nvidiaEnabled bool) (*VfioPciManager, error) {
	iommuEnabled, err := checkIommuEnabled(nvlib.hostRoot)
	if err != nil {
		return nil, fmt.Errorf("error checking if IOMMU is enabled: %w", err)
	}
	if !iommuEnabled {
		return nil, fmt.Errorf("IOMMU is not enabled in the kernel")
	}

	vm := &VfioPciManager{
		containerDriverRoot:    containerDriverRoot,
		hostDriverRoot:         hostDriverRoot,
		nvlib:                  nvlib,
		nvidiaEnabled:          nvidiaEnabled,
		inflightDriverSwitches: make(map[string]struct{}),
	}

	return vm, nil
}

// Configure binds the GPU to the vfio-pci driver.
func (vm *VfioPciManager) Configure(ctx context.Context, info *VfioDeviceInfo) error {
	loaded, err := vm.checkKernelModuleLoaded(info.vfioModule)
	if err != nil {
		return fmt.Errorf("error checking if module %q is loaded: %w", info.vfioModule, err)
	}
	if !loaded {
		err := vm.loadKernelModule(info.vfioModule)
		if err != nil {
			return fmt.Errorf("failed to load module %q: %w", info.vfioModule, err)
		}
	}

	// If we were already in the middle of changing drivers, then don't proceed.
	if vm.isDriverSwitchInflight(info.PciBusID) {
		return fmt.Errorf("a driver switch is inflight for GPU %q, please check the kernel logs for more details", info.PciBusID)
	}

	vfioDriver, err := getVfioDriverName(info.vfioModule)
	if err != nil {
		return fmt.Errorf("error getting vfio driver for GPU %q: %w", info.PciBusID, err)
	}

	driver, err := getDriver(pciDevicesPath, info.PciBusID)
	if err != nil {
		return fmt.Errorf("error getting driver details for GPU %q: %w", info.PciBusID, err)
	}

	// Skip if the GPU is already bound to the vfio-pci (or variant) driver.
	if driver == vfioDriver {
		return nil
	}

	// Only support vfio-pci (or variant) or nvidia (if vm.nvidiaEnabled) driver.
	if vm.nvidiaEnabled {
		// The driver may be empty if a previous driver change operation got aborted
		// before completion. changeDriver() is idempotent and handles this scenario.
		if driver != "" && driver != nvidiaDriver {
			return fmt.Errorf("GPU %q is bound to %q driver, expected %q or %q", info.PciBusID, driver, vfioDriver, nvidiaDriver)
		}
	}

	// Verify SRIOV VFs are disabled on the GPU.
	err = vm.verifyDisabledVFs(info.PciBusID)
	if err != nil {
		return fmt.Errorf("error verifying disabled VFs: %w", err)
	}

	if driver == nvidiaDriver {
		// Disable GPU Persistence Mode.
		err = vm.disableGPUPersistenceMode(info.PciBusID)
		if err != nil {
			return fmt.Errorf("error disabling persistence mode for GPU %q: %w", info.PciBusID, err)
		}

		// Wait for other GPU clients to evacuate.
		err = vm.WaitForGPUFree(ctx, info)
		if err != nil {
			return fmt.Errorf("error waiting for GPU %q to be free: %w", info.PciBusID, err)
		}
	}

	// Change the GPU driver to vfio-pci (or variant).
	// Run the driver change operation in a separate goroutine because the
	// underlying kernel operation may get stuck and cannot be cancelled. The
	// driver change holds the inflight marker until it completes so that a
	// subsequent Configure/Unconfigure call will not attempt to change the
	// driver again.
	err = vm.tryChangeDriverWithTimeout(ctx, driverChangeTimeout, func() error {
		return vm.changeDriver(info.PciBusID, vfioDriver)
	})
	if err != nil {
		return fmt.Errorf("error changing driver for GPU %q: %w", info.PciBusID, err)
	}

	return nil
}

// WaitForGPUFree does a best effort scan of the GPU clients running on the host and
// waits for them to exit on their own.
//
// This polls the GPU's /dev/nvidia* device node in the driver installation path on
// the host periodically to see if any process has open fds to it. This acts as a
// limited safety net to ensure that we don't mistakenly try to unbind a GPU from
// the nvidia driver while it is busy.
// Note: Here, we can only check if there are any GPU clients running on the host rootfs
// where the driver is installed. If you have containerized GPU clients that work
// with their own view of the device nodes, we will not able to detect it.
func (vm *VfioPciManager) WaitForGPUFree(ctx context.Context, info *VfioDeviceInfo) error {
	if info.parent == nil {
		return nil
	}
	timeout := time.After(gpuFreeCheckTimeout)
	ticker := time.NewTicker(gpuFreeCheckInterval)
	defer ticker.Stop()

	gpuDeviceNode := filepath.Join(vm.hostDriverRoot, "dev", fmt.Sprintf("nvidia%d", info.parent.minor))
	var err error
	for {
		select {
		case <-timeout:
			return fmt.Errorf("timed out waiting for gpu to be free: %w", err)
		case <-ticker.C:
			out, cmdErr := execCommandWithChroot(vm.nvlib.hostRoot, "fuser", []string{gpuDeviceNode}) //nolint:gosec
			if cmdErr != nil {
				// fuser returns exit code 1 if no process is using the device.
				if exitErr, ok := cmdErr.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
					return nil
				}
				err = fmt.Errorf("unexpected error checking if gpu device %q is free: %w", info.PciBusID, cmdErr)
				klog.V(6).Infof("%v", err)
				continue
			}
			err = fmt.Errorf("gpu device %q has open fds by process(es): %q", info.PciBusID, string(out))
			klog.V(6).Infof("%v", err)
		}
	}
}

// Unconfigure binds the GPU to the nvidia driver.
func (vm *VfioPciManager) Unconfigure(ctx context.Context, info *VfioDeviceInfo) error {
	// Do nothing if we dont expect to switch to nvidia driver.
	if !vm.nvidiaEnabled {
		return nil
	}

	// Change the GPU driver to nvidia.
	err := vm.changeDriver(info.PciBusID, nvidiaDriver)
	if err != nil {
		return fmt.Errorf("error changing driver for GPU %q: %w", info.PciBusID, err)
	}

	// Enable GPU Persistence Mode.
	err = vm.enableGPUPersistenceMode(info.PciBusID)
	if err != nil {
		return fmt.Errorf("error enabling persistence mode for GPU %q: %w", info.PciBusID, err)
	}

	return nil
}

// Module names in the modules.alias file will only ever contain underscore
// characters and not dashes -- this aligns with how the linux kernel
// stores module names internally. This can sometimes differ from the name of the
// directory in /sys/bus/pci/drivers/ for a given module. For example, this
// contradiction exists for the standard vfio-pci module:
//
// $ file /sys/bus/pci/drivers/vfio-pci
// sys/bus/pci/drivers/vfio-pci: directory
//
// $ modinfo vfio-pci | grep ^name:
// name:           vfio_pci
//
// To account for this difference, we check if the module name exists in
// /sys/bus/pci/drivers, and if not, we try again but with any underscore
// characters converted to dashes.
func getVfioDriverName(vfioModule string) (string, error) {
	vfioDriver := vfioModule
	if _, err := os.Stat(filepath.Join(pciDriversPath, vfioDriver)); err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			return "", fmt.Errorf("unexpected error checking driver directory for vfio module %q at %q: %w", vfioModule, pciDriversPath, err)
		}
		vfioDriverNormalized := strings.ReplaceAll(vfioDriver, "_", "-")
		if _, err := os.Stat(filepath.Join(pciDriversPath, vfioDriverNormalized)); err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				return "", fmt.Errorf("unexpected error checking driver directory for vfio module %q at %q: %w", vfioModule, pciDriversPath, err)
			}
			return "", fmt.Errorf("failed to find driver directory for vfio module %q at %q, is the module loaded?", vfioModule, pciDriversPath)
		}
		vfioDriver = vfioDriverNormalized
	}
	return vfioDriver, nil
}

// Get the current driver the GPU is bound to.
func getDriver(pciDevicesPath, pciAddress string) (string, error) {
	driverPath, err := os.Readlink(filepath.Join(pciDevicesPath, pciAddress, "driver"))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", nil
		}
		return "", err
	}
	_, driver := filepath.Split(driverPath)
	return driver, nil
}

// Run the driver change operation in a separate goroutine and wait for it to complete.
//
// If the driver change times out, its possible that it may be stuck in the kernel
// and become uncancelable due to another process holding an open handle to the
// GPU device minor file. Another consequence of this is that the plugin
// process may not be able to terminate due to the stuck kernel task. This would
// require either the GPU handle to be released by the culprit process or a node
// reboot to recover. This is executed with a fixed timeout to avoid holding the
// `pu.lock` when running in the context of the goroutine serving the
// NodePrepare/NodeUnprepare API call.
func (vm *VfioPciManager) tryChangeDriverWithTimeout(ctx context.Context, timeout time.Duration, changeDriverFn func() error) error {
	timeoutCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Create a buffered channel to avoid blocking the goroutine
	// if this function times out and exits.
	errCh := make(chan error, 1)
	go func() {
		defer close(errCh)
		errCh <- changeDriverFn()
	}()

	select {
	case err := <-errCh:
		return err
	case <-timeoutCtx.Done():
		return timeoutCtx.Err()
	}
}

// Change the driver the GPU is bound to.
//
// Here, we keep track of inflight driver switches so that we don't attempt to
// switch the driver again while a previous driver switch for a GPU is in progress.
// A previous driver switch if stuck for some reason is expected to be stalled
// in the kernel and hence we error out early without reattempting it. The
// goroutine responsible for this stuck operation will also block any attempt to
// terminate the plugin process. Once the goroutine is freed up, we're safe to
// reattempt the driver switch.
func (vm *VfioPciManager) changeDriver(pciAddress, driver string) error {
	err := vm.markDriverSwitchInflight(pciAddress)
	if err != nil {
		return err
	}
	defer vm.unmarkDriverSwitchInflight(pciAddress)

	currentDriver, err := getDriver(pciDevicesPath, pciAddress)
	if err != nil {
		return fmt.Errorf("error getting driver details for GPU %q: %w", pciAddress, err)
	}

	// Skip if the GPU is already bound to the desired driver.
	if currentDriver == driver {
		return nil
	}

	if currentDriver != "" {
		err := vm.nvlib.nvpasst.Unbind(pciAddress)
		if err != nil {
			return fmt.Errorf("error unbinding GPU %q from driver %q: %w", pciAddress, currentDriver, err)
		}
	}

	err = vm.nvlib.nvpasst.BindToDriver(pciAddress, driver)
	if err != nil {
		return fmt.Errorf("error binding GPU %q to driver %q: %w", pciAddress, driver, err)
	}
	return nil
}

// Check if a driver switch is inflight for the given PCI address.
func (vm *VfioPciManager) isDriverSwitchInflight(pciAddress string) bool {
	vm.Lock()
	defer vm.Unlock()
	_, exists := vm.inflightDriverSwitches[pciAddress]
	return exists
}

// Mark a driver switch as inflight for the given PCI address.
func (vm *VfioPciManager) markDriverSwitchInflight(pciAddress string) error {
	vm.Lock()
	defer vm.Unlock()
	if _, exists := vm.inflightDriverSwitches[pciAddress]; exists {
		return fmt.Errorf("an existing driver switch for GPU %q is inflight, please check the kernel logs for more details", pciAddress)
	}

	vm.inflightDriverSwitches[pciAddress] = struct{}{}
	return nil
}

// Unmark inflight driver switch for the given PCI address.
func (vm *VfioPciManager) unmarkDriverSwitchInflight(pciAddress string) {
	vm.Lock()
	defer vm.Unlock()
	delete(vm.inflightDriverSwitches, pciAddress)
}

// Verify there are no VFs on the GPU.
func (vm *VfioPciManager) verifyDisabledVFs(pciBusID string) error {
	gpu, err := vm.nvlib.nvpci.GetGPUByPciBusID(pciBusID)
	if err != nil {
		return err
	}
	if gpu == nil {
		return fmt.Errorf("no GPU found at PCI bus ID %q", pciBusID)
	}
	// PhysicalFunction is nil for GPUs that do not support SR-IOV (e.g. T400).
	// A nil PhysicalFunction means no VFs can exist, so it is safe to proceed.
	if gpu.SriovInfo.PhysicalFunction == nil {
		return nil
	}
	numVFs := gpu.SriovInfo.PhysicalFunction.NumVFs
	if numVFs > 0 {
		return fmt.Errorf("gpu has %d VFs, cannot unbind", numVFs)
	}
	return nil
}

// Enable GPU Persistence Mode.
func (vm *VfioPciManager) enableGPUPersistenceMode(pciAddress string) error {
	// Obtain a lock to serialize persistence mode operations.
	// This is a cautious approach to avoid any NVML race conditions.
	vm.Lock()
	defer vm.Unlock()
	return vm.nvlib.enableGPUPersistenceMode(pciAddress)
}

// Disable GPU Persistence Mode.
func (vm *VfioPciManager) disableGPUPersistenceMode(pciAddress string) error {
	// Obtain a lock to serialize persistence mode operations.
	// This is a cautious approach to avoid any NVML race conditions.
	vm.Lock()
	defer vm.Unlock()
	// We dont need to toggle persistence mode if nvidia-persistenced is not running.
	klog.V(4).Infof("Checking if nvidia-persistenced is running: %s", filepath.Join(vm.containerDriverRoot, nvidiaPersistencedSocketPath))
	_, err := os.Stat(filepath.Join(vm.containerDriverRoot, nvidiaPersistencedSocketPath))
	if err != nil {
		if !os.IsNotExist(err) {
			return fmt.Errorf("error checking if nvidia-persistenced is running: %w", err)
		}
		klog.V(4).Infof("nvidia-persistenced is not running; nothing to do...")
		return nil
	}

	err = vm.nvlib.disableGPUPersistenceMode(pciAddress)
	if err != nil {
		return fmt.Errorf("error disabling persistence mode for GPU %q: %w", pciAddress, err)
	}
	return nil
}

// Check if the expected kernel module is loaded.
func (vm *VfioPciManager) checkKernelModuleLoaded(module string) (bool, error) {
	f, err := os.Stat(filepath.Join(vm.nvlib.hostRoot, sysModulePath, module))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("failed to check if module %q is loaded: %w", module, err)
	}

	if !f.IsDir() {
		return false, nil
	}

	return true, nil
}

// Load given kernel module.
func (vm *VfioPciManager) loadKernelModule(module string) error {
	_, err := execCommandWithChroot(vm.nvlib.hostRoot, "modprobe", []string{module}) //nolint:gosec
	if err != nil {
		return err
	}

	return nil
}

// Check if IOMMU is enabled.
func checkIommuEnabled(hostRoot string) (bool, error) {
	f, err := os.Open(filepath.Join(hostRoot, kernelIommuGroupPath))
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer f.Close()
	_, err = f.Readdirnames(1)
	if err == io.EOF {
		return false, nil
	}
	if err != nil {
		return false, err
	}

	return true, nil
}

// Check if IOMMUFD is enabled.
// We correlate the IOMMUFD support with the presence of the /dev/iommu API device.
func checkIommuFDEnabled(hostRoot string) (bool, error) {
	_, err := os.Stat(filepath.Join(hostRoot, iommuDevicePath))
	if err != nil {
		if os.IsNotExist(err) {
			klog.Infof("IOMMUFD is not enabled, /dev/iommu device node does not exist")
			return false, nil
		}
		return false, fmt.Errorf("error checking if iommu device node exists: %w", err)
	}
	return true, nil
}

// Execute a command with chroot.
func execCommandWithChroot(fsRoot, cmd string, args []string) ([]byte, error) {
	chrootArgs := []string{fsRoot, cmd}
	chrootArgs = append(chrootArgs, args...)
	return exec.Command("chroot", chrootArgs...).CombinedOutput()
}
