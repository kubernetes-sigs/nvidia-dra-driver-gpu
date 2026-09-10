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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/NVIDIA/go-nvml/pkg/nvml"
	"github.com/pmezard/go-difflib/difflib"
	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/dynamic-resource-allocation/kubeletplugin"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/kubelet/checkpointmanager"
	cperrors "k8s.io/kubernetes/pkg/kubelet/checkpointmanager/errors"
	"k8s.io/utils/ptr"
	cdiapi "tags.cncf.io/container-device-interface/pkg/cdi"

	"github.com/sirupsen/logrus"

	configapi "sigs.k8s.io/dra-driver-nvidia-gpu/api/nvidia.com/resource/v1beta1"
	"sigs.k8s.io/dra-driver-nvidia-gpu/internal/common"
	"sigs.k8s.io/dra-driver-nvidia-gpu/internal/lookup/root"
	"sigs.k8s.io/dra-driver-nvidia-gpu/pkg/bootid"
	"sigs.k8s.io/dra-driver-nvidia-gpu/pkg/fabricmanager"
	"sigs.k8s.io/dra-driver-nvidia-gpu/pkg/featuregates"
	"sigs.k8s.io/dra-driver-nvidia-gpu/pkg/flock"
	drametrics "sigs.k8s.io/dra-driver-nvidia-gpu/pkg/metrics"
)

type OpaqueDeviceConfig struct {
	Requests []string
	Config   runtime.Object
}

type DeviceConfigState struct {
	MpsControlDaemonID string              `json:"mpsControlDaemonID"`
	MpsApplied         *bool               `json:"mpsApplied,omitempty"`
	Config             configapi.Interface `json:"-"` // don't serialize this.
	containerEdits     *cdiapi.ContainerEdits
	TimeSliceApplied   *bool `json:"timeSliceApplied,omitempty"`
}

type DeviceState struct {
	sync.Mutex
	cdi                      *CDIHandler
	tsManager                *TimeSlicingManager
	mpsManager               *MpsManager
	vfioPciManager           *VfioPciManager
	checkpointCleanupManager *CheckpointCleanupManager
	config                   *Config

	// Allocatable devices grouped by physical GPU.
	// This is useful for grouped announcement
	// (e.g., when announcing one ResourceSlice per physical GPU).
	perGPUAllocatable *PerGPUAllocatableDevices

	nvdevlib          *deviceLib
	checkpointManager checkpointmanager.CheckpointManager

	fmManager *fabricmanager.Manager

	// Checkpoint read/write lock, file-based for multi-process synchronization.
	cplock *flock.Flock
}

// newFabricManager opens a Fabric Manager connection when the
// FabricManagerPartitioning feature is enabled and this node has an
// NVSwitch/NVLink fabric. It returns (nil, nil) when Fabric Manager is not
// applicable (feature disabled, mock NVML in use, or no fabric detected).
func newFabricManager(nvdevlib *deviceLib, driver *root.Driver) (*fabricmanager.Manager, error) {
	if !featuregates.Enabled(featuregates.FabricManagerPartitioning) {
		return nil, nil
	}

	if common.UsingAltProcDevices() {
		klog.Infof("Mock NVML proc is in use; skipping Fabric Manager")
		return nil, nil
	}

	hasFabric, err := fabricmanager.HasFabricManagerFabric(nvdevlib.hostRoot)
	if err != nil {
		return nil, fmt.Errorf("FabricManagerPartitioning enabled but fabric detection failed: %w", err)
	}
	if !hasFabric {
		klog.Infof("FabricManagerPartitioning enabled but no NVSwitch/NVLink5 fabric detected on this node; skipping Fabric Manager")
		return nil, nil
	}

	libPath, err := driver.LibraryPath("libnvfm.so")
	if err != nil {
		return nil, fmt.Errorf("FabricManagerPartitioning enabled but fabric manager library not found: %w", err)
	}
	fmManager, err := fabricmanager.OpenFabricManager(libPath)
	if err != nil {
		return nil, fmt.Errorf("FabricManagerPartitioning enabled but Fabric Manager could not be opened: %w", err)
	}
	return fmManager, nil
}

func NewDeviceState(ctx context.Context, config *Config) (*DeviceState, error) {
	driver := root.New(root.WithDriverRoot(config.flags.containerDriverRoot))
	devRoot := driver.DevRoot
	klog.Infof("Using devRoot=%v", devRoot)

	nvdevlib, err := newDeviceLib(driver, config.flags.hostRoot)
	if err != nil {
		return nil, fmt.Errorf("failed to create device library: %w", err)
	}

	perGPUAllocatable, err := nvdevlib.enumerateAllPossibleDevices()
	if err != nil {
		return nil, fmt.Errorf("error enumerating all possible devices: %w", err)
	}

	hostDriverRoot := config.flags.hostDriverRoot

	// Let nvcdi logs see the light of day (emit to standard streams) when we've
	// been configured with verbosity level 7 or higher.
	cdilogger := logrus.New()
	if config.flags.klogVerbosity < 7 {
		klog.Infof("Muting CDI logger (verbosity is smaller 7: %d)", config.flags.klogVerbosity)
		cdilogger.SetOutput(io.Discard)
	}

	cdiOptions := []cdiOption{
		WithNvml(nvdevlib.nvmllib),
		WithDeviceLib(nvdevlib),
		WithDriverRoot(driver.Root),
		WithDevRoot(devRoot),
		WithTargetDriverRoot(hostDriverRoot),
		WithNVIDIACDIHookPath(config.flags.nvidiaCDIHookPath),
		WithCDIRoot(config.flags.cdiRoot),
		WithLogger(cdilogger),
	}
	var vfioCDIHandler *vfioCDIHandler
	if featuregates.Enabled(featuregates.PassthroughSupport) {
		vfioCDIHandler, err = NewVfioCDIHandler(nvdevlib)
		if err != nil {
			return nil, fmt.Errorf("unable to create vfio CDI handler: %w", err)
		}
		cdiOptions = append(cdiOptions, WithVfioCDIHandler(vfioCDIHandler))
	}
	cdi, err := NewCDIHandler(cdiOptions...)
	if err != nil {
		return nil, fmt.Errorf("unable to create CDI handler: %w", err)
	}

	var fullGPUuuids []string
	for _, devices := range perGPUAllocatable.allocatablesMap {
		for _, dev := range devices {
			if dev.Gpu != nil {
				fullGPUuuids = append(fullGPUuuids, dev.Gpu.UUID)
			}
		}
	}

	klog.V(2).Infof("Warming up CDI device spec cache for GPUs %v", fullGPUuuids)
	cdi.WarmupDevSpecCache(fullGPUuuids)

	var tsManager *TimeSlicingManager
	if featuregates.Enabled(featuregates.TimeSlicingSettings) {
		tsManager = NewTimeSlicingManager(nvdevlib)
	}

	var mpsManager *MpsManager
	if featuregates.Enabled(featuregates.MPSSupport) {
		mpsManager = NewMpsManager(config, nvdevlib, hostDriverRoot, MpsControlDaemonTemplatePath)
	}

	var vfioPciManager *VfioPciManager
	if featuregates.Enabled(featuregates.PassthroughSupport) {
		vfioPciManager, err = NewVfioPciManager(driver.Root, hostDriverRoot, nvdevlib, true /* nvidiaEnabled */)
		if err != nil {
			return nil, fmt.Errorf("unable to create vfio pci manager: %w", err)
		}
	}

	fmManager, err := newFabricManager(nvdevlib, driver)
	if err != nil {
		return nil, err
	}

	checkpointManager, err := checkpointmanager.NewCheckpointManager(config.DriverPluginPath())
	if err != nil {
		return nil, fmt.Errorf("unable to create checkpoint manager: %w", err)
	}

	cpLockPath := filepath.Join(config.DriverPluginPath(), "cp.lock")

	state := &DeviceState{
		cdi:               cdi,
		tsManager:         tsManager,
		mpsManager:        mpsManager,
		vfioPciManager:    vfioPciManager,
		perGPUAllocatable: perGPUAllocatable,
		config:            config,
		nvdevlib:          nvdevlib,
		checkpointManager: checkpointManager,
		fmManager:         fmManager,
		cplock:            flock.NewFlock(cpLockPath),
	}
	state.checkpointCleanupManager = NewCheckpointCleanupManager(state, config.clientsets.Resource)

	// Attach Fabric Manager partition mappings to every discovered GPU. The
	// gpuModuleID was resolved from NVML during GPU discovery; the FM Manager
	// (owned by DeviceState) turns it into the size->partitionId mapping that
	// VFIO devices publish via their parent GpuInfo.
	if state.fabricManagerPartitioningEnabled() {
		for _, gpu := range nvdevlib.gpuInfosByUUID {
			if err := state.attachFabricManagerPartitions(gpu); err != nil {
				return nil, fmt.Errorf("attaching fabric manager partitions for GPU %s: %w", gpu.CanonicalName(), err)
			}
		}
	}

	checkpoints, err := state.checkpointManager.ListCheckpoints()
	if err != nil {
		return nil, fmt.Errorf("unable to list checkpoints: %w", err)
	}

	currentBootID, err := bootid.GetCurrentBootID()
	if err != nil {
		return nil, fmt.Errorf("read node boot id: %w", err)
	}

	for _, c := range checkpoints {
		if c == DriverPluginCheckpointFileBasename {
			cp, err := state.getCheckpoint(ctx)
			if err != nil {
				return nil, fmt.Errorf("unable to get checkpoint: %w", err)
			}
			storedBootID := cp.GetNodeBootID()
			if storedBootID == "" { //nolint:gocritic,staticcheck
				// legacy checkpoint file does not contain a boot ID, inject current boot ID
				// note: this is a temporary workaround to ensure that the checkpoint file is always updated with the current boot ID
				// note: this will temporary break the assertion that its prepared devices are prepared by the same boot ID
				klog.V(4).Info("The existing checkpoint file does not contain a boot ID, injecting current boot ID")
				err := state.updateCheckpoint(ctx, func(checkpoint *Checkpoint) {
					checkpoint.V2.NodeBootID = currentBootID
				})
				if err != nil {
					return nil, fmt.Errorf("unable to update checkpoint: %w", err)
				}
				syncPreparedDevicesGaugeFromCheckpoint(config.flags.nodeName, cp)
				return state, nil
			} else if storedBootID == currentBootID {
				syncPreparedDevicesGaugeFromCheckpoint(config.flags.nodeName, cp)
				return state, nil
			} else {
				klog.Infof("Invalidating checkpoint: checkpoint nodeBootID %q != current %q", storedBootID, currentBootID)
			}
		}
	}

	klog.Infof("Create empty checkpoint")
	newCheckpoint := &Checkpoint{V2: &CheckpointV2{NodeBootID: currentBootID}}
	if err := state.createCheckpoint(ctx, newCheckpoint); err != nil {
		return nil, fmt.Errorf("unable to create fresh checkpoint: %w", err)
	}

	return state, nil
}

func (s *DeviceState) Prepare(ctx context.Context, claim *resourceapi.ResourceClaim) ([]kubeletplugin.Device, error) {

	if err := s.validateAdminAccessRequest(claim); err != nil {
		return nil, err
	}

	tplock0 := time.Now()
	s.Lock()
	defer s.Unlock()
	klog.V(6).Infof("t_prep_state_lock_acq %.3f s", time.Since(tplock0).Seconds())

	claimUID := string(claim.UID)

	tgcp0 := time.Now()
	cp, err := s.getCheckpoint(ctx)
	if err != nil {
		return nil, fmt.Errorf("unable to get checkpoint: %w", err)
	}
	klog.V(7).Infof("t_prep_get_checkpoint %.3f s", time.Since(tgcp0).Seconds())

	// Check for existing 'completed' claim preparation before updating the
	// checkpoint with 'PrepareStarted'. Otherwise, we effectively mark a
	// perfectly prepared claim as only partially prepared, which may have
	// negative side effects during Unprepare() (currently a noop in this case:
	// unprepare noop: claim preparation started but not completed).
	preparedClaim, exists := cp.V2.PreparedClaims[claimUID]
	if exists && preparedClaim.CheckpointState == ClaimCheckpointStatePrepareCompleted {
		// Make this a noop. Associated device(s) has/ave been prepared by us.
		// Prepare() must be idempotent, as it may be invoked more than once per
		// claim (and actual device preparation must happen at most once).
		klog.V(4).Infof("Skip prepare: claim already in PrepareCompleted state: %s", ResourceClaimToString(claim))
		return preparedClaim.PreparedDevices.GetDevices(), nil
	}

	// In certain scenarios, the same device can be prepared/allocated more than once for different claims
	// due to races between data processing in different goroutines in the scheduler, or when pods are
	// force-deleted while the kubelet still considers the devices allocated.
	// To prevent this, we check whether any device requested in the incoming claim has already been prepared
	// and fail the request if so (unless the prior preparation was performed with admin access).
	// More details: https://github.com/kubernetes/kubernetes/pull/136269
	if err := s.validateNoOverlappingPreparedDevices(cp, claim); err != nil {
		return nil, fmt.Errorf("unable to prepare claim %v: %w", claimUID, err)
	}

	// Relevant for DynamicMIG: a previous preparation attempt for the same
	// claim might have resulted in complete or partial GI/CI creation. Roll
	// that back, and retry creation from scratch (that maybe can later be
	// optimized into filling the gaps).
	if exists && preparedClaim.CheckpointState == ClaimCheckpointStatePrepareStarted {
		klog.V(4).Infof("Claim %s already in PrepareStarted state: attempt rollback before new prepare", ResourceClaimToString(claim))
		if err := s.rollbackPartiallyPreparedClaim(ctx, claimUID, preparedClaim, cp); err != nil {
			return nil, fmt.Errorf("failed to roll back partially prepared claim %s: %w", PreparedClaimToString(&preparedClaim, claimUID), err)
		}
	}

	tucp0 := time.Now()
	err = s.updateCheckpoint(ctx, func(cp *Checkpoint) {
		cp.V2.PreparedClaims[claimUID] = PreparedClaim{
			CheckpointState: ClaimCheckpointStatePrepareStarted,
			Status:          claim.Status,
			Name:            claim.Name,
			Namespace:       claim.Namespace,
		}
	})
	if err != nil {
		return nil, fmt.Errorf("unable to update checkpoint: %w", err)
	}
	klog.V(6).Infof("t_prep_update_checkpoint %.3f s", time.Since(tucp0).Seconds())
	klog.V(6).Infof("checkpoint updated for claim %v", claimUID)

	tprep0 := time.Now()
	preparedDevices, err := s.prepareDevices(ctx, claim, cp)
	klog.V(6).Infof("t_prep_core %.3f s (claim %s)", time.Since(tprep0).Seconds(), ResourceClaimToString(claim))
	if err != nil {
		return nil, fmt.Errorf("failed to prepare devices: %w", err)
	}

	// TODO: Remove this once partitionable device support is introduced for vfio devices.
	if featuregates.Enabled(featuregates.PassthroughSupport) {
		for _, device := range preparedDevices.GetDevices() {
			allocatableDevice := s.perGPUAllocatable.GetAllocatableDevice(device.DeviceName)
			if allocatableDevice == nil {
				klog.Warningf("allocatable not found for device: %v", device.DeviceName)
				continue
			}
			// Remove all other device types on the parent GPU of the prepared device.
			// When vfio type is prepared, gpu type should not be advertised and vice versa.
			s.perGPUAllocatable.RemoveSiblingDevices(allocatableDevice)
		}
	}

	tccsf0 := time.Now()
	if err := s.cdi.CreateClaimSpecFile(claimUID, preparedDevices); err != nil {
		return nil, fmt.Errorf("unable to create CDI spec file for claim: %w", err)
	}
	klog.V(7).Infof("t_prep_ccsf %.3f s", time.Since(tccsf0).Seconds())

	tucp20 := time.Now()
	err = s.updateCheckpoint(ctx, func(cp *Checkpoint) {
		cp.V2.PreparedClaims[claimUID] = PreparedClaim{
			CheckpointState: ClaimCheckpointStatePrepareCompleted,
			Status:          claim.Status,
			PreparedDevices: preparedDevices,
		}
	})
	if err != nil {
		return nil, fmt.Errorf("unable to update checkpoint: %w", err)
	}
	klog.V(6).Infof("checkpoint updated for claim %v", claimUID)
	klog.V(7).Infof("t_prep_ucp2 %.3f s", time.Since(tucp20).Seconds())

	return preparedDevices.GetDevices(), nil
}

// DestroyUnknownMIGDevices() relies on the checkpoint as the source of truth.
// It tries to tear down any existing MIG device that is not referenced by
// currently checkpointed claims in ClaimCheckpointStatePrepareCompleted state.
//
// This routine is currently called once during startup before accepting
// requests from the kubelet. TODO: a better mechanism to make sure it does not
// conflict with overlapping Prepare() and Unprepare() activities (from e.g. a
// separate plugin process, during an upgrade) would be to hold on to file-based
// lock on the checkpoint (during the entire operation). Notworthy: the
// operation may potentially take significant wall time.
//
// We need to learn over time how workload running on a to-be-torn-down MIG
// device may and should affect the teardown attempt.
//
// There may be a fundamental need to invoke this type of cleanup periodically,
// instead of once during startup.
//
// Note: there are other cleanup strategies performed at runtime: (1) periodic
// cleanup of partially prepared but _stale_ claims (2) attempted rollback in
// the Prepare() path for a specific, non-stale claim). Especially once/if this
// method here is executed periodically, there is overlap with (1) -- but there
// are decisive differences:
//
// - This routine here (0) does not mutate the checkpoint. It only tears down
// devices. (1) removes entries from the checkpoint after confirming cleanup.
// (1) may also tear down MIG devices that (0) would otherwise tear down: one
// wins, and that is OK.
//
// - (2) is something that (1) cannot achieve, because the claim in question is
// not stale. A previous Prepare() attempt was executed partially and any state
// mutations performed may make an immediately retried Prepare() fail (with
// "insufficient resources"). In that case, (2) performs an active rollback in
// the business logic of the retried Prepare() call, to help resolve the
// conflict quickly (from the user's point of view). (0) may also help but not
// in a timely fashion.
//
// All cleanup strategies are critical but also dangerous -- when designed or
// implemented wrongly, they may affect workload.
//
// Significance of this cleanup:
//
// 1) Healing out-of-band MIG device creation. Forbidden by design (make sure to
// document tha). Sometimes, MIG devices however still get created manually,
// out-of-band). It is convenient for the system to recover from that
// automatically.
//
// 2) Recovering from interruptions within a multi-step transaction (as of bugs
// within this plugin, issues in the NVML management layer, invasive operations,
// etc). There is a lot of room, especially over time, for state to be drifting
// as of partially performed transactions.
func (s *DeviceState) DestroyUnknownMIGDevices(ctx context.Context) {
	logpfx := "Destroy unknown MIG devices"
	cp, err := s.getCheckpoint(ctx)
	if err != nil {
		klog.Errorf("%s: unable to get checkpoint: %s", logpfx, err)
		return
	}

	// Get checkpointed claims in PrepareCompleted state. Only MIG devices
	// referenced in those will be retained. That is, the below's logic tears
	// down MIG devices that correspond to claims in a PrepareStarted limbo
	// state -- that can only be correct when there are no overlapping
	// NodePrepareResources() executions.
	filtered := make(PreparedClaimsByUIDV2)
	for uid, claim := range cp.V2.PreparedClaims {
		if claim.CheckpointState == ClaimCheckpointStatePrepareCompleted {
			filtered[uid] = claim
		}
	}

	var expectedDeviceNames []DeviceName
	for _, cpclaim := range filtered {
		for _, res := range cpclaim.Status.Allocation.Devices.Results {
			expectedDeviceNames = append(expectedDeviceNames, res.Device)
		}
	}

	klog.Infof("%s: enter teardown routine (%d expect devices: %s)", logpfx, len(expectedDeviceNames), expectedDeviceNames)

	// For now, let this be best-effort. Upon error, proceed with the program,
	// do not crash it. TODO: maybe this should be timeout-controlled.
	if err := s.nvdevlib.obliterateStaleMIGDevices(expectedDeviceNames); err != nil {
		klog.Errorf("%s: obliterateStaleMIGDevices failed: %s", logpfx, err)
	}

	klog.Infof("%s: done", logpfx)
}

// Unprepare returns true when cleanup removes a Dynamic MIG XID taint and the
// caller must republish the ResourceSlice.
// TODO: May be replace this bool with a general resource-change result if Unprepare
// starts reporting other changes that require republishing.
func (s *DeviceState) Unprepare(ctx context.Context, claimRef kubeletplugin.NamespacedObject) (bool, error) {
	s.Lock()
	defer s.Unlock()
	klog.V(6).Infof("Unprepare() for claim '%s'", claimRef.String())

	checkpoint, err := s.getCheckpoint(ctx)
	if err != nil {
		return false, fmt.Errorf("failed to get checkpoint: %w", err)
	}

	claimUID := string(claimRef.UID)
	pc, exists := checkpoint.V2.PreparedClaims[claimUID]
	if !exists {
		// Not an error: if this claim UID is not in the checkpoint then this
		// device was never prepared or has already been unprepared (assume that
		// Prepare+Checkpoint are done transactionally). Note that
		// claimRef.String() contains namespace, name, UID.
		klog.V(2).Infof("Unprepare noop: claim not found in checkpoint data: %v", claimRef.String())
		return false, nil
	}

	var taintRemoved bool
	switch pc.CheckpointState {
	case ClaimCheckpointStatePrepareStarted:
		if err := s.unpreparePartiallyPreparedClaim(ctx, claimUID, pc, checkpoint); err != nil {
			return false, fmt.Errorf("failed to unprepare partially prepared claim %s: %w", claimRef.String(), err)
		}
	case ClaimCheckpointStatePrepareCompleted:
		taintRemoved, err = s.unprepareDevices(ctx, claimUID, pc.PreparedDevices, checkpoint)
		if err != nil {
			return false, fmt.Errorf("failed to unprepare devices for claim %s: %w", claimRef.String(), err)
		}
	default:
		return false, fmt.Errorf("unsupported ClaimCheckpointState: %v", pc.CheckpointState)
	}

	// TODO: Remove this once partitionable device support is introduced for vfio devices.
	if featuregates.Enabled(featuregates.PassthroughSupport) {
		for _, device := range pc.PreparedDevices.GetDevices() {
			allocatableDevice := s.perGPUAllocatable.GetAllocatableDevice(device.DeviceName)
			if allocatableDevice == nil {
				klog.Warningf("allocatable not found for device: %v", device.DeviceName)
				continue
			}
			// If consumable shares is enabled and this shared GPU/MIG device is still in use
			// by other active container claims, do not re-advertise its sibling VFIO device yet.
			if isConsumableSharesEnabled(s.config) {
				var inUse bool
				if allocatableDevice.Type() == GpuDeviceType && allocatableDevice.Gpu != nil {
					inUse = isGpuUUIDInUseByOtherClaims(checkpoint, claimUID, allocatableDevice.Gpu.UUID)
				} else if allocatableDevice.IsStaticOrDynMigDevice() {
					var migUUID string
					if preparedMig := s.getPreparedMigDevice(checkpoint, device.DeviceName); preparedMig != nil && preparedMig.Concrete != nil {
						migUUID = preparedMig.Concrete.MigUUID
					}
					inUse = isMigDeviceInUseByOtherClaims(checkpoint, claimUID, migUUID, device.DeviceName)
				}
				if inUse {
					klog.V(4).Infof("Unprepare: device %s still in use by other claims, skipping sibling rediscovery", device.DeviceName)
					continue
				}
			}
			// Rediscover all sibling devices on the parent GPU of the unprepared device.
			// When vfio type is unprepared, gpu type should be advertised and vice versa.
			// For a VFIO device this also repopulates its parent GpuInfo (now
			// back on the nvidia driver) with fresh Fabric Manager info.
			err := s.discoverSiblingAllocatables(allocatableDevice)
			if err != nil {
				return false, fmt.Errorf("error discovering sibling allocatables: %w", err)
			}
		}
	}
	if s.fabricManagerPartitioningEnabled() {
		if err := s.deactivateFabricPartition(claimUID, &pc, checkpoint); err != nil {
			return false, fmt.Errorf("error deactivating fabric partition: %w", err)
		}
	}

	// TODOMIG: we delete per-claim CDI spec files here in the happy path. In
	// regular operation, that means we don't leak files. However, upon program
	// start, or periodically, clean up CDI spec directory just in case we're
	// ever missing or failing to delete (a) file(s).
	if err := s.cdi.DeleteClaimSpecFile(claimUID); err != nil {
		// Just log an error -- if this fails, we still want to proceed
		// attempting to remove the claim from the checkpoint.
		klog.Errorf("unable to delete CDI spec file for claim %s: %s", claimRef.String(), err)
	}

	// Mutate checkpoint reflecting that all devices for this claim have been
	// unprepared, by virtue of removing its entry (based on claim UID) from the
	// PreparedClaims map.
	err = s.deleteClaimFromCheckpoint(ctx, claimRef)
	if err != nil {
		return false, fmt.Errorf("error deleting claim from checkpoint: %w", err)
	}
	return taintRemoved, nil
}

// Rollback previously partially prepared claim.
//
// This is called when a previous Prepare() attempt was partially performed and
// failed. We rollback the partially prepared claim before re-attempting the
// prepare workflow.
//
// Note: We do not attempt rollback of VFIO devices during Prepare() as its
// device configuration is idempotent.
func (s *DeviceState) rollbackPartiallyPreparedClaim(ctx context.Context, cuid string, pc PreparedClaim, checkpoint *Checkpoint) error {
	// Attempt rollback of MIG devices if DynamicMIG is enabled.
	if featuregates.Enabled(featuregates.DynamicMIG) {
		allocDevsForClaim := s.getAllocatableDevicesForClaim(cuid, pc)
		migDevices := allocDevsForClaim.GetMigDynamicDevices()
		if len(migDevices) > 0 {
			klog.V(2).Infof("unprepare: MIG rollback for partially prepared claim %s (devices: %d)", PreparedClaimToString(&pc, cuid), len(migDevices))

			err := s.rollbackPartiallyPreparedMIGDevices(ctx, cuid, pc, checkpoint)
			if err != nil {
				return fmt.Errorf("failed to roll back partially prepared MIG devices: %w", err)
			}
		}
	}

	return nil
}

// Unprepare previously partially prepared claim.
//
// This is called during Unprepare() for a claim that is known to be stale (not in the API
// server or terminating). Here, the `checkpoint` data is fresh enough; there is no other
// that currently legitimately owns the device represented in `pc`. This usually happens
// when the Prepare() call failed or did not finish and we want to clean up the
// `PrepareStarted` claim.
func (s *DeviceState) unpreparePartiallyPreparedClaim(ctx context.Context, cuid string, pc PreparedClaim, checkpoint *Checkpoint) error {
	allocDevsForClaim := s.getAllocatableDevicesForClaim(cuid, pc)

	// Attempt rollback of MIG devices if DynamicMIG is enabled.
	if featuregates.Enabled(featuregates.DynamicMIG) {
		migDevices := allocDevsForClaim.GetMigDynamicDevices()
		if len(migDevices) > 0 {
			klog.V(2).Infof("unprepare: MIG rollback for partially prepared claim %s (devices: %d)", PreparedClaimToString(&pc, cuid), len(migDevices))

			err := s.rollbackPartiallyPreparedMIGDevices(ctx, cuid, pc, checkpoint)
			if err != nil {
				return fmt.Errorf("failed to roll back partially prepared MIG devices: %w", err)
			}
		}
	}

	// Attempt rollback of VFIO devices if PassthroughSupport is enabled.
	if featuregates.Enabled(featuregates.PassthroughSupport) {
		vfioDevices := allocDevsForClaim.GetVfioDevices()
		if len(vfioDevices) > 0 {
			klog.V(2).Infof("unprepare: VFIO rollback for partially prepared claim %s (devices: %d)", PreparedClaimToString(&pc, cuid), len(vfioDevices))

			err := s.rollbackPartiallyPreparedVFIODevices(ctx, vfioDevices)
			if err != nil {
				return fmt.Errorf("failed to roll back partially prepared VFIO devices: %w", err)
			}
		}
	}

	// If FM partitioning is enabled, then deactivate the partition
	// for the devices in the claim. At this point, the gpuInfo objects
	// for all devices in the claim are expected to have been discovered.
	// Note: This is only relevant for GPU/VFIO devices and the operation
	// itself is idempotent so even if the partitions were never
	// activated, its safe to call this function and it'll be a no-op.
	if s.fabricManagerPartitioningEnabled() {
		if err := s.deactivateFabricPartition(cuid, &pc, checkpoint); err != nil {
			return fmt.Errorf("error deactivating fabric partition: %w", err)
		}
	}

	return nil
}

// getAllocatableDevicesForClaim returns the allocatable devices for a given
// checkpointed claim.
func (s *DeviceState) getAllocatableDevicesForClaim(claimUID string, pc PreparedClaim) AllocatableDevices {
	allocDevsForClaim := make(AllocatableDevices)

	if pc.Status.Allocation == nil {
		return allocDevsForClaim
	}

	for _, r := range pc.Status.Allocation.Devices.Results {
		if r.Driver != DriverName {
			continue
		}
		device := s.perGPUAllocatable.GetAllocatableDevice(r.Device)
		if device == nil {
			// The allocatable may legitimately be absent, e.g. the sibling
			// GPU was already rediscovered by a previous rollback attempt.
			klog.V(4).Infof("Partial unprepare: allocatable not found for device %q (claim %s); skipping", r.Device, PreparedClaimToString(&pc, claimUID))
			continue
		}
		allocDevsForClaim[r.Device] = device
	}
	return allocDevsForClaim
}

// Revert previous and potentially partial MIG device creation (not acknowledged
// by transitioning the claim state to PrepareCompleted). Can we safely revert
// that based on the information in checkpoint? As we didn't pull through with
// the regular creation/prepare flow, we conceptually cannot use a specific MIG
// device UUID as input to this cleanup operation. The precise physical MIG
// device configuration however is known: it is encoded in the canonical device
// name. That is, we have the chance to reliably identify and tear down an
// orphaned MIG device (which was created by us, but never got user workload
// assigned). It is absolutely critical, however, that this cleanup method does
// not accidentally tear down the wrong device. At the time of writing, I
// believe that the correct way to achieve that is to verify that currently no
// other claim in the PrepareCompleted state refers to the same device.
//
// The information available to us here is sparse -- for example:
//
//	"checkpointState": "PrepareStarted", "status": {
//	  "allocation": {
//	    "devices": {
//	      "results": [
//	        {
//	          "request": "mig-1g",
//	          "driver": "gpu.nvidia.com",
//	          "pool": "gb-nvl-027-compute06",
//	          "device": "gpu-1-mig-2g47gb-14-0"
//	        }
//	      ]
//	    }
//
// Also note that a plugin restart would tear down such device as part of its
// `DestroyUnknownMIGDevices()` (which is executed before the driver accepts
// requests).
//
// Construct a list of those claims that, according to the current snapshot,
// have been properly prepared.
func (s *DeviceState) rollbackPartiallyPreparedMIGDevices(ctx context.Context, claimUID string, pc PreparedClaim, checkpoint *Checkpoint) error {
	// When DynamicMIG is enabled, try to identify an orphaned MIG device
	// corresponding to `pc`. To that end, inspect which currently (completely)
	// prepared claims use which devices.
	completedClaims := make(PreparedClaimsByUIDV2)
	for cuid, c := range checkpoint.V2.PreparedClaims {
		if c.CheckpointState == ClaimCheckpointStatePrepareCompleted {
			completedClaims[cuid] = c
		}
	}

	for _, r := range pc.Status.Allocation.Devices.Results {
		devname := r.Device
		ms, err := NewMigSpecTupleFromCanonicalName(devname)
		if err != nil {
			// This may be a regular, full GPU -- in which case there's nothing
			// to do. To be sure that we detect parser errors, log that error
			// though.
			klog.V(6).Infof("Device name %s failed NewMigSpecTupleFromCanonicalName() parsing (assume this is not a MIG device): %s", devname, err)
			continue
		}

		klog.V(1).Infof("Device %s is a MIG device, DynamicMIG mode: deleteMigDevIfExistsAndNotUsedByCompletedClaim()", devname)
		if err := s.deleteMigDevIfExistsAndNotUsedByCompletedClaim(ms, devname, completedClaims); err != nil {
			return fmt.Errorf("failed to delete unused MIG device %s (deleteMigDevIfExistsAndNotUsedByCompletedClaim): %w", devname, err)
		}
	}

	return nil
}

// Rollback VFIO passthrough side effects. A previous Prepare() attempt may
// have already bound one or more GPUs to vfio-pci (or variant). Unlike the
// PrepareCompleted state where we have checkpointed PreparedDevices, a
// partially prepared claim has no PreparedDevices checkpointed yet, so we
// retrieve the affected VFIO devices from the allocation results and mirror
// the completed-path teardown.
//
// Note: For VFIO devices in this state, it may be possible that a driver
// change operation has not completed during the past Prepare() call. In this
// case, rollback will fail until the driver change operation is unclogged.
func (s *DeviceState) rollbackPartiallyPreparedVFIODevices(ctx context.Context, vfioDevices []*AllocatableDevice) error {
	if len(vfioDevices) == 0 {
		return nil
	}

	for _, device := range vfioDevices {
		if device.Type() != VfioDeviceType {
			continue
		}

		info := device.Vfio
		// Unconfigure() is idempotent and is expected to revert any
		// changes from the past Configure() call.
		if err := s.vfioPciManager.Unconfigure(ctx, info); err != nil {
			return fmt.Errorf("error unconfiguring vfio device %q: %w", info.CanonicalName(), err)
		}

		// Rediscover all siblings of the VFIO device now that the GPU
		// is back on the nvidia driver.
		if err := s.discoverSiblingAllocatables(device); err != nil {
			return fmt.Errorf("error discovering sibling allocatables for vfio device %q: %w", info.CanonicalName(), err)
		}
	}

	return nil
}

func (s *DeviceState) createCheckpoint(ctx context.Context, cp *Checkpoint) error {
	klog.V(6).Info("acquire cplock (create cp)")
	release, err := s.cplock.Acquire(ctx, flock.WithTimeout(10*time.Second))
	if err != nil {
		return fmt.Errorf("error acquiring cplock: %w", err)
	}
	defer release()
	klog.V(7).Info("acquired cplock (createCheckpoint)")
	err = s.checkpointManager.CreateCheckpoint(DriverPluginCheckpointFileBasename, cp)
	klog.V(7).Info("create cp: done")
	if err != nil {
		return err
	}
	syncPreparedDevicesGaugeFromCheckpoint(s.config.flags.nodeName, cp)
	return nil
}

func (s *DeviceState) getCheckpoint(ctx context.Context) (*Checkpoint, error) {
	klog.V(7).Info("acquire cplock (getCheckpoint)")
	release, err := s.cplock.Acquire(ctx, flock.WithTimeout(10*time.Second))
	if err != nil {
		return nil, fmt.Errorf("error acquiring cplock: %w", err)
	}
	defer release()
	klog.V(7).Info("acquired cplock (getCheckpoint)")

	checkpoint := &Checkpoint{}
	if err := s.checkpointManager.GetCheckpoint(DriverPluginCheckpointFileBasename, checkpoint); err != nil {
		if errors.Is(err, cperrors.CorruptCheckpointError{}) {
			logCheckpointDiff(s.config.DriverPluginPath(), checkpoint)
		}
		return nil, err
	}

	klog.V(7).Info("checkpoint read")
	return checkpoint.ToLatestVersion(), nil
}

// logCheckpointDiff is invoked when GetCheckpoint returns
// CorruptCheckpointError: the on-disk JSON deserialized cleanly but the
// checksum recomputed at verification time disagrees with the one encoded in
// the file. The typical cause is a backward-incompatible field addition
// somewhere in the checkpoint type graph (see issue 1080). Emit a unified diff
// between the on-disk bytes and what the current binary would re-marshal, so an
// operator can spot the offending fields immediately.
func logCheckpointDiff(driverPluginPath string, deserialized *Checkpoint) {
	ondisk, rerr := os.ReadFile(filepath.Join(driverPluginPath, DriverPluginCheckpointFileBasename))
	remarshaled, merr := json.Marshal(deserialized)
	if rerr != nil || merr != nil {
		klog.Errorf("checkpoint failed checksum verification; diagnostic dump unavailable (readErr=%v, marshalErr=%v)", rerr, merr)
		return
	}
	var a, b bytes.Buffer
	_ = json.Indent(&a, ondisk, "", "  ")
	_ = json.Indent(&b, remarshaled, "", "  ")
	diff, derr := difflib.GetUnifiedDiffString(difflib.UnifiedDiff{
		A:        strings.SplitAfter(a.String(), "\n"),
		B:        strings.SplitAfter(b.String(), "\n"),
		FromFile: "on-disk",
		ToFile:   "re-marshaled",
		Context:  3,
	})
	if derr != nil {
		klog.Errorf("checkpoint failed checksum verification; diff computation failed: %v. on-disk: %s. re-marshaled: %s", derr, ondisk, remarshaled)
		return
	}
	klog.Errorf("checkpoint failed checksum verification; unified diff (on-disk vs re-marshaled by current binary):\n%s", diff)
}

// Read checkpoint from store, perform mutation, and write checkpoint back. Any
// mutation of the checkpoint must go through this function. Perform the
// read-mutate-write sequence under a dedicated lock: we must be conceptually
// certain that multiple read-mutate-write actions never overlap. Currently,
// this is also ensured by the global PU lock -- but this inner lock is
// explicit, tested, and can replace the global PU lock if desired.
func (s *DeviceState) updateCheckpoint(ctx context.Context, mutate func(*Checkpoint)) error {
	tucp0 := time.Now()
	klog.V(7).Info("acquire cplock (updateCheckpoint)")
	release, err := s.cplock.Acquire(ctx, flock.WithTimeout(10*time.Second))
	if err != nil {
		return fmt.Errorf("error acquiring cplock: %w", err)
	}
	defer release()
	klog.V(7).Info("acquired cplock (updateCheckpoint)")

	checkpoint := &Checkpoint{}
	if err := s.checkpointManager.GetCheckpoint(DriverPluginCheckpointFileBasename, checkpoint); err != nil {
		return fmt.Errorf("updateCheckpoint: unable to get checkpoint: %w", err)
	}

	// Potentially migrate to newest version. This also creates an empty
	// `PreparedClaims` map if that field is so far `nil` (so that insertion is
	// always safe). This is also called in the getCheckpoint() helper.
	cp := checkpoint.ToLatestVersion()
	mutate(cp)

	err = s.checkpointManager.CreateCheckpoint(DriverPluginCheckpointFileBasename, cp)
	if err != nil {
		return fmt.Errorf("unable to create checkpoint: %w", err)
	}
	klog.V(6).Infof("t_checkpoint_update_total %.3f s", time.Since(tucp0).Seconds())
	syncPreparedDevicesGaugeFromCheckpoint(s.config.flags.nodeName, cp)
	return nil
}

func (s *DeviceState) deleteClaimFromCheckpoint(ctx context.Context, claimRef kubeletplugin.NamespacedObject) error {
	err := s.updateCheckpoint(ctx, func(cp *Checkpoint) {
		delete(cp.V2.PreparedClaims, string(claimRef.UID))
	})
	if err != nil {
		return fmt.Errorf("unable to update checkpoint: %w", err)
	}
	klog.V(6).Infof("Deleted claim from checkpoint: %s", claimRef.String())
	return nil
}

// validateAdminAccessRequest rejects admin-access requests that are incompatible
// with how this driver treats admin access. Admin access is meant for host-side
// monitoring/management of a full GPU, so it must not be combined with:
//   - a non-full-GPU device (e.g. a VFIO passthrough or MIG device), or
//   - any device configuration that applies to the admin-access request.
func (s *DeviceState) validateAdminAccessRequest(claim *resourceapi.ResourceClaim) error {
	if claim.Status.Allocation == nil {
		return nil
	}

	// Reject admin access on any device that is not a full GPU.
	adminRequests := make(map[string]struct{})
	for _, r := range claim.Status.Allocation.Devices.Results {
		if r.Driver != DriverName {
			continue
		}
		if r.AdminAccess == nil || !*r.AdminAccess {
			continue
		}
		adminRequests[r.Request] = struct{}{}
		device := s.perGPUAllocatable.GetAllocatableDevice(r.Device)
		if device != nil && device.Type() != GpuDeviceType {
			return fmt.Errorf("claim %s requests admin access on a non-full-GPU device, which is not supported", ResourceClaimToString(claim))
		}
	}

	if len(adminRequests) == 0 {
		return nil
	}

	// Reject admin access combined with any device configuration for this driver
	// that applies to an admin-access request.
	for _, cfg := range claim.Status.Allocation.Devices.Config {
		if cfg.Opaque == nil || cfg.Opaque.Driver != DriverName {
			continue
		}
		// An empty Requests list means the config applies to every request in the
		// claim, including the admin-access ones.
		if len(cfg.Requests) == 0 {
			return fmt.Errorf("claim %s requests admin access with a device configuration, which is not supported", ResourceClaimToString(claim))
		}
		for _, req := range cfg.Requests {
			if _, ok := adminRequests[req]; ok {
				return fmt.Errorf("claim %s requests admin access with a device configuration, which is not supported", ResourceClaimToString(claim))
			}
		}
	}
	return nil
}

func (s *DeviceState) prepareDevices(ctx context.Context, claim *resourceapi.ResourceClaim, cp *Checkpoint) (PreparedDevices, error) {
	if claim.Status.Allocation == nil {
		return nil, fmt.Errorf("claim not yet allocated")
	}

	klog.V(6).Infof("Preparing devices for claim %s", ResourceClaimToString(claim))

	configResultsMap, err := s.getConfigResultsMap(claim.Status.Allocation)
	if err != nil {
		return nil, err
	}

	if featuregates.Enabled(featuregates.PassthroughSupport) {
		vfioGroups := 0
		for c := range configResultsMap {
			if _, ok := c.(*configapi.VfioDeviceConfig); ok {
				vfioGroups++
			}
		}
		if vfioGroups > 1 {
			return nil, fmt.Errorf("claim %s contains %d VFIO device groups, but at most one is supported per claim", ResourceClaimToString(claim), vfioGroups)
		}
	}

	if s.fabricManagerPartitioningEnabled() {
		if err := s.activateFabricPartition(claim); err != nil {
			return nil, err
		}
	}

	// Normalize, validate, and apply all configs associated with devices that
	// need to be prepared. Track device group configs generated from applying the
	// config to the set of device allocation results.
	preparedDeviceGroupConfigState := make(map[runtime.Object]*DeviceConfigState)
	for c, results := range configResultsMap {
		// Cast the opaque config to a configapi.Interface type
		var config configapi.Interface
		switch castConfig := c.(type) {
		case *configapi.GpuConfig:
			config = castConfig
		case *configapi.MigDeviceConfig:
			config = castConfig
		case *configapi.VfioDeviceConfig:
			config = castConfig
		default:
			return nil, fmt.Errorf("runtime object is not a recognized configuration")
		}

		// Normalize the config to set any implied defaults.
		if err := config.Normalize(); err != nil {
			return nil, fmt.Errorf("error normalizing GPU config: %w", err)
		}

		// Validate the config to ensure its integrity.
		if err := config.Validate(); err != nil {
			return nil, fmt.Errorf("error validating GPU config: %w", err)
		}

		// Apply the config to the list of results associated with it. If this
		// applies to a DynamicMIG device then at this point the device has not
		// yet been created (i.e., the UUID of the MIG device is not yet known).
		configState, err := s.applyConfig(ctx, config, claim, results, cp)
		if err != nil {
			return nil, fmt.Errorf("error applying config: %w", err)
		}

		// Capture the prepared device group config in the map.
		preparedDeviceGroupConfigState[c] = configState
	}

	// Walk through each config and its associated device allocation results
	// and construct the list of prepared devices to return.
	var preparedDevices PreparedDevices
	for c, results := range configResultsMap {
		preparedDeviceGroup := PreparedDeviceGroup{
			ConfigState: *preparedDeviceGroupConfigState[c],
		}

		for _, result := range results {
			cdiDevices := []string{}
			allocatableDevice := s.perGPUAllocatable.GetAllocatableDevice(result.Device)
			if allocatableDevice == nil {
				return nil, fmt.Errorf("allocatable not found for device %q", result.Device)
			}
			// The claim-specific CDI spec (of kind `k8s.gpu.nvidia.com/claim`)
			// has not yet been generated. But we already know the name of a
			// ClaimDevice entry that it will enumerate (by convention).
			if d := s.cdi.GetClaimDeviceName(string(claim.UID), allocatableDevice, preparedDeviceGroupConfigState[c].containerEdits); d != "" {
				cdiDevices = append(cdiDevices, d)
			}

			device := &CheckpointedDevice{
				Requests:     []string{result.Request},
				PoolName:     result.Pool,
				DeviceName:   result.Device,
				CDIDeviceIDs: cdiDevices,
			}

			// KEP-5304: Add device metadata to the prepared devices.
			if featuregates.Enabled(featuregates.DeviceMetadata) {
				if allocatableDevice.Type() == VfioDeviceType {
					attrs := make(map[string]resourceapi.DeviceAttribute)
					for k, v := range allocatableDevice.Vfio.GetDevice().Attributes {
						attrs[string(k)] = v
					}
					device.Metadata = &kubeletplugin.DeviceMetadata{
						Attributes: attrs,
					}
				}
			}

			var preparedDevice PreparedDevice

			switch allocatableDevice.Type() {
			case GpuDeviceType:
				preparedDevice.Gpu = &PreparedGpu{
					Info:   allocatableDevice.Gpu,
					Device: device,
				}
			case MigStaticDeviceType:
				preparedDevice.Mig = &PreparedMigDevice{
					Concrete: allocatableDevice.MigStatic.LiveTuple(),
					Device:   device,
				}
			case MigDynamicDeviceType:
				migspec := allocatableDevice.MigDynamic
				if existingMig := s.getPreparedMigDevice(cp, device.DeviceName); existingMig != nil {
					preparedDevice.Mig = &PreparedMigDevice{
						Concrete: existingMig.Concrete,
						Device:   device,
					}
				} else {
					// Note: immediately after createMigDevice() returns, we could
					// persist data to disk that may be useful for cleaning up a
					// partial prepare more reliably (such as the MIG device UUID).
					tcmig0 := time.Now()
					migdev, err := s.nvdevlib.createMigDevice(migspec)
					klog.V(6).Infof("t_prep_create_mig_dev %.3f s (claim %s)", time.Since(tcmig0).Seconds(), ResourceClaimToString(claim))
					if err != nil {
						return nil, fmt.Errorf("error creating MIG device: %w", err)
					}
					preparedDevice.Mig = &PreparedMigDevice{
						Concrete: migdev.LiveTuple(),
						Device:   device,
					}
				}
			case VfioDeviceType:
				preparedDevice.Vfio = &PreparedVfioDevice{
					Info:   allocatableDevice.Vfio,
					Device: device,
				}
			}

			klog.V(6).Infof("Prepared device for claim '%s': %s", ResourceClaimToString(claim), device.DeviceName)
			// Here is a unique opportunity to update the checkpoint, reflecting
			// the current state of device preparation (still within
			// PrepareStarted state, but with more detail than before). There is
			// potential for crashes and early termination between here and
			// final claim preparation.
			preparedDeviceGroup.Devices = append(preparedDeviceGroup.Devices, preparedDevice)
		}

		preparedDevices = append(preparedDevices, &preparedDeviceGroup)
	}

	return preparedDevices, nil
}

func (s *DeviceState) unprepareDevices(ctx context.Context, claimUID string, devices PreparedDevices, checkpoint *Checkpoint) (bool, error) {
	klog.V(6).Infof("Unpreparing claim '%s', previously prepared devices from checkpoint: %v", claimUID, devices.GetDeviceNames())
	var taintRemoved bool
	for _, group := range devices {
		// Unconfigure the vfio-pci devices.
		if featuregates.Enabled(featuregates.PassthroughSupport) {
			err := s.unprepareVfioDevices(ctx, group.Devices.VfioDevices())
			if err != nil {
				return false, fmt.Errorf("error unpreparing VFIO devices: %w", err)
			}
		}

		// TODOMIG: do this after MPS/TimeSlicing primitive teardown?
		for _, device := range group.Devices {
			switch device.Type() {
			case GpuDeviceType:
				klog.V(4).Infof("Unprepare: regular GPU: noop (GPU %s)", device.Gpu.Info.String())
			case PreparedMigDeviceType:
				if featuregates.Enabled(featuregates.DynamicMIG) {
					mig := device.Mig.Concrete
					if isConsumableSharesEnabled(s.config) && isMigDeviceInUseByOtherClaims(checkpoint, claimUID, mig.MigUUID, device.Mig.Device.DeviceName) {
						klog.V(4).Infof("Unprepare: dynamic MIG device '%s' still in use by other claims, skipping deletion", mig.MigUUID)
						continue
					}
					klog.V(4).Infof("Unprepare: tear down MIG device '%s' for claim '%s'", mig.MigUUID, claimUID)
					// Errors during MIG device deletion are generally rare but
					// have to be expected, and should fail the
					// NodeUnprepareResources() operation. This may for example
					// be 'error destroying GPU Instance: In use by another
					// client' and resolve itself soon; when the conflicting
					// party goes away. Log an explicit warning, in addition to
					// returning an error.
					err := s.nvdevlib.deleteMigDevice(mig)
					if err != nil {
						klog.Warningf("Error deleting MIG device %s (UUID %s): %s", device.Mig.Device.DeviceName, mig.MigUUID, err)
						return false, fmt.Errorf("failed to delete MIG device %s (UUID %s): %w", device.Mig.Device.DeviceName, mig.MigUUID, err)
					}
					if s.clearDynamicMIGXIDTaint(device.Mig.Device.DeviceName) {
						taintRemoved = true
					}
				} else {
					klog.V(4).Infof("Unprepare: static MIG: noop (MIG %s)", device.Mig.Concrete.MigUUID)
				}
			}
		}

		// Stop any MPS control daemons started for each group of prepared devices.
		if featuregates.Enabled(featuregates.MPSSupport) {
			mpsControlDaemon := s.mpsManager.NewMpsControlDaemon(claimUID, group)
			if err := mpsControlDaemon.Stop(ctx); err != nil {
				return false, fmt.Errorf("error stopping MPS control daemon: %w", err)
			}
		}
		// Reset when time-slicing was applied at prepare (true), or when the
		// checkpoint predates timeSliceApplied (nil — legacy implicit time-slicing).
		if featuregates.Enabled(featuregates.TimeSlicingSettings) &&
			ptr.Deref(group.ConfigState.TimeSliceApplied, true) {
			var gpuUUIDsToReset []string
			for _, gpuUUID := range group.Devices.GpuUUIDs() {
				if isConsumableSharesEnabled(s.config) && isGpuUUIDInUseByOtherClaims(checkpoint, claimUID, gpuUUID) {
					klog.V(4).Infof("Unprepare: GPU %s still in use by other claims, skipping time-slice reset", gpuUUID)
				} else {
					gpuUUIDsToReset = append(gpuUUIDsToReset, gpuUUID)
				}
			}
			if len(gpuUUIDsToReset) > 0 {
				defaultInterval := configapi.DefaultTimeSlice
				tsc := &configapi.TimeSlicingConfig{Interval: &defaultInterval}
				if err := s.tsManager.SetTimeSlice(gpuUUIDsToReset, tsc); err != nil {
					if err == nvml.ERROR_NOT_SUPPORTED {
						klog.Warningf("Unprepare: skip resetting time-slice policy for devices: %v", err)
					} else {
						return false, fmt.Errorf("error setting timeslice for devices: %w", err)
					}
				}
			}
		}

	}
	return taintRemoved, nil
}

// unprepareVfioDevices rebinds each passthrough GPU from vfio-pci back to the
// nvidia driver. It intentionally does NOT deactivate the Fabric Manager
// partition: deactivation is deferred to the caller until after the parent
// GpuInfo has been repopulated (via discoverSiblingAllocatables) with a fresh
// gpuModuleID resolved from NVML now that the GPU is visible again.
func (s *DeviceState) unprepareVfioDevices(ctx context.Context, devices PreparedDeviceList) error {
	for _, device := range devices {
		vfioAllocatable := s.perGPUAllocatable.GetAllocatableDevice(device.Vfio.Device.DeviceName)
		if vfioAllocatable == nil {
			return fmt.Errorf("allocatable not found for vfio device %q", device.Vfio.Device.DeviceName)
		}
		if err := s.vfioPciManager.Unconfigure(ctx, vfioAllocatable.Vfio); err != nil {
			return fmt.Errorf("error unconfiguring vfio device %q: %w", vfioAllocatable.Vfio.CanonicalName(), err)
		}
	}
	return nil
}

// Discover all sibling devices on the parent GPU of the given device.
//
// When a GPU is prepared in passthrough mode as a vfio device, the GPU may no longer be used on the nvidia driver,
// so MIGs and GPU devices are no longer available on the node. When the GPU is unprepared and put back on the nvidia driver,
// we need to rediscover the GPU as certain nvidia driver-specific attributes (such as device minors) may have changed.
// This function needs to be called to rediscover these attributes, if any.
// TODO: Support MIGs as part of the PartitionableDevices integration with the PassthroughSupport feature gate.
func (s *DeviceState) discoverSiblingAllocatables(device *AllocatableDevice) error {
	switch device.Type() {
	case GpuDeviceType:
		if !device.Gpu.vfioEnabled {
			return nil
		}
		vfioAllocatable, err := s.nvdevlib.discoverVfioDevice(device.Gpu)
		if err != nil {
			return fmt.Errorf("error discovering vfio device: %w", err)
		}
		err = s.perGPUAllocatable.AddAllocatableDevice(vfioAllocatable)
		if err != nil {
			return fmt.Errorf("error adding allocatable device: %w", err)
		}
	case VfioDeviceType:
		gpu, _, err := s.nvdevlib.discoverGPUByPCIBusID(device.Vfio.PciBusID)
		if err != nil {
			return fmt.Errorf("error discovering gpu by pci bus id: %w", err)
		}
		err = s.perGPUAllocatable.AddAllocatableDevice(gpu)
		if err != nil {
			return fmt.Errorf("error adding allocatable device: %w", err)
		}
		device.Vfio.parent = gpu.Gpu

		// The GPU is back on the nvidia driver: its freshly discovered parent
		// GpuInfo already carries the gpuModuleID (resolved from NVML in
		// getGpuInfo). Attach the FM partition mapping.
		if err := s.attachFabricManagerPartitions(gpu.Gpu); err != nil {
			return fmt.Errorf("error attaching fabric manager partitions for gpu %q: %w", gpu.Gpu.CanonicalName(), err)
		}
	case MigStaticDeviceType:
		// TODO: Implement once partitionable device is supported with PassthroughSupport feature gate.
		return nil
	case MigDynamicDeviceType:
		// TODO: Implement once partitionable device is supported with PassthroughSupport feature gate.
		return nil
	}
	return nil
}

func (s *DeviceState) applyConfig(ctx context.Context, config configapi.Interface, claim *resourceapi.ResourceClaim, results []*resourceapi.DeviceRequestAllocationResult, cp *Checkpoint) (*DeviceConfigState, error) {
	switch castConfig := config.(type) {
	case *configapi.GpuConfig:
		klog.V(7).Infof("applySharingConfig() for GpuConfig")
		return s.applySharingConfig(ctx, castConfig.Sharing, claim, results, cp)
	case *configapi.MigDeviceConfig:
		klog.V(7).Infof("applySharingConfig() for MigDeviceConfig")
		return s.applySharingConfig(ctx, castConfig.Sharing, claim, results, cp)
	case *configapi.VfioDeviceConfig:
		klog.V(7).Infof("applySharingConfig() for VfioDeviceConfig")
		return s.applyVfioDeviceConfig(ctx, castConfig, claim, results)
	default:
		return nil, fmt.Errorf("unknown config type: %T", castConfig)
	}
}

func (s *DeviceState) applySharingConfig(ctx context.Context, config configapi.Sharing, claim *resourceapi.ResourceClaim, results []*resourceapi.DeviceRequestAllocationResult, cp *Checkpoint) (*DeviceConfigState, error) {
	// Get the list of claim requests this config is being applied over.
	var requests []string
	for _, r := range results {
		requests = append(requests, r.Request)
	}

	// Get the list of allocatable devices this config is being applied over.
	requestedDevices := make(AllocatableDevices)
	for _, r := range results {
		device := s.perGPUAllocatable.GetAllocatableDevice(r.Device)
		if device == nil {
			return nil, fmt.Errorf("allocatable not found for device %q", r.Device)
		}
		requestedDevices[r.Device] = device
	}

	// Declare a device group state object to populate.
	var configState DeviceConfigState

	// Apply time-slicing settings (if available and feature gate enabled).
	if featuregates.Enabled(featuregates.TimeSlicingSettings) && config.IsTimeSlicing() {
		tsc, err := config.GetTimeSlicingConfig()
		if err != nil {
			return nil, fmt.Errorf("error getting timeslice config for requests '%v' in claim '%v': %w", requests, claim.UID, err)
		}
		if tsc != nil {
			// Get UUIDs of physical GPUs for which to change the timeslicing
			// settings. Do this only for any requested full GPUs. Note:
			// timeslicing settings cannot be set on MIG devices directly.
			// Hence, the API does not allow for setting `timeSlicingConfig` on
			// a `MigDeviceConfig`. TODO: should we do this for passthrough
			// devices?
			uuids := requestedDevices.GpuUUIDs()
			klog.V(6).Infof("SetTimeSlice() for full GPUs with UUIDs: %s", uuids)
			err = s.tsManager.SetTimeSlice(uuids, tsc)
			if err != nil {
				return nil, fmt.Errorf("error setting timeslice config for requests '%v' in claim '%v': %w", requests, claim.UID, err)
			}
			configState.TimeSliceApplied = ptr.To(true)
		}
	}

	// Apply MPS settings (if available and feature gate enabled).
	if featuregates.Enabled(featuregates.MPSSupport) && config.IsMps() {
		if isConsumableSharesEnabled(s.config) {
			return nil, fmt.Errorf("MPS sharing is not supported when consumable shares is enabled")
		}
		if featuregates.Enabled(featuregates.DynamicMIG) {
			// TODO: create MIG device first, get its UUID, and then enable MPS
			// for that device -- probably based on a `PreparedDevicesList`, and
			// not based on `AllocatableDevices`.
			return nil, fmt.Errorf("MPS is not yet supported when using featureGates.DynamicMIG=true")
		}
		mpsc, err := config.GetMpsConfig()
		if err != nil {
			return nil, fmt.Errorf("error getting MPS configuration: %w", err)
		}
		mpsControlDaemon := s.mpsManager.NewMpsControlDaemon(string(claim.UID), requestedDevices)
		if err := mpsControlDaemon.Start(ctx, mpsc); err != nil {
			return nil, fmt.Errorf("error starting MPS control daemon: %w", err)
		}
		if err := mpsControlDaemon.AssertReady(ctx); err != nil {
			return nil, fmt.Errorf("MPS control daemon is not yet ready: %w", err)
		}
		configState.MpsControlDaemonID = mpsControlDaemon.GetID()
		configState.containerEdits = mpsControlDaemon.GetCDIContainerEdits()
	}

	return &configState, nil
}

func (s *DeviceState) applyVfioDeviceConfig(ctx context.Context, config *configapi.VfioDeviceConfig, claim *resourceapi.ResourceClaim, results []*resourceapi.DeviceRequestAllocationResult) (*DeviceConfigState, error) {
	configState := DeviceConfigState{
		Config: config,
	}

	// Add common vfio device container edits to the group config. This is
	// configuring vfio devices only for a specific group. Its assumed that
	// there is only a single vfio device configuration applied in the
	// resourceclaim for all its requests. This means there's only a single
	// prepared devices group and these edits are never duplicated. This
	// implicitly assumptes that other device types are not present in the
	// resourceclaim allocation.
	commonEdits, err := s.cdi.vfiocdi.GetCommonEdits(config.Iommu.ShouldEnableAPIDevice(), config.Iommu.ShouldPreferIommuFD())
	if err != nil {
		return nil, fmt.Errorf("error getting common vfio device container edits: %w", err)
	}

	configState.containerEdits = commonEdits

	infos := make([]*VfioDeviceInfo, 0, len(results))
	for _, r := range results {
		device := s.perGPUAllocatable.GetAllocatableDevice(r.Device)
		if device == nil {
			return nil, fmt.Errorf("allocatable not found for vfio device %q", r.Device)
		}
		infos = append(infos, device.Vfio)
	}

	for i, r := range results {
		if err := s.vfioPciManager.Configure(ctx, infos[i]); err != nil {
			return nil, fmt.Errorf("error configuring vfio device %q: %w", r.Device, err)
		}
	}

	return &configState, nil
}

// resolveFabricPartition resolves the FM partition formed by the given
// set of physical GPUs, using each GPU's gpuModuleID (resolved from NVML at
// discovery).
func (s *DeviceState) resolveFabricPartition(gpus []*GpuInfo) (int, error) {
	moduleIDs := make([]int, 0, len(gpus))
	for _, gpu := range gpus {
		if gpu == nil || gpu.gpuModuleID == 0 {
			pci := ""
			if gpu != nil {
				pci = gpu.pciBusID
			}
			return 0, fmt.Errorf("fabric manager: no gpuModuleID for GPU at PCI %q", pci)
		}
		moduleIDs = append(moduleIDs, gpu.gpuModuleID)
	}
	partitionID, ok := s.fmManager.FindPartitionByModuleIDs(moduleIDs)
	if !ok {
		return 0, fmt.Errorf("fabric manager: GPU module set %v does not match any FM partition", moduleIDs)
	}
	return partitionID, nil
}

// gpuInfosFromPreparedClaim maps a claim's device allocation results to the set of
// backing physical GPUs for full GPUs and VFIO devices.
func (s *DeviceState) gpuInfosFromPreparedClaim(results []resourceapi.DeviceRequestAllocationResult) []*GpuInfo {
	var gpus []*GpuInfo
	for _, r := range results {
		if r.Driver != DriverName {
			continue
		}
		device := s.perGPUAllocatable.GetAllocatableDevice(r.Device)
		if device == nil {
			klog.V(6).Infof("allocatable not found for device %q", r.Device)
			continue
		}
		switch device.Type() {
		case GpuDeviceType:
			gpus = append(gpus, device.Gpu)
		case VfioDeviceType:
			gpus = append(gpus, device.Vfio.parent)
		default:
			klog.V(6).Infof("device %q has unsupported type %q; skipping for fabric partition", r.Device, device.Type())
		}
	}
	return gpus
}

// deactivateFabricPartition releases the FM partition formed by the physical
// GPUs backing the claim's allocation results. It is a no-op when Fabric
// Manager partitioning is disabled or when the claim has no allocation.
// Callers that include VFIO devices must first rebind those GPUs to the nvidia
// driver and rediscover so each VFIO device's parent is repopulated.
func (s *DeviceState) deactivateFabricPartition(claimUID string, pc *PreparedClaim, checkpoint *Checkpoint) error {
	if !s.fabricManagerPartitioningEnabled() || pc.Status.Allocation == nil || isAdminAccess(pc.Status.Allocation.Devices.Results) {
		return nil
	}
	gpus := s.gpuInfosFromPreparedClaim(pc.Status.Allocation.Devices.Results)
	if len(gpus) == 0 {
		return nil
	}
	if isConsumableSharesEnabled(s.config) {
		for _, gpu := range gpus {
			if gpu != nil && isGpuUUIDInUseByOtherClaims(checkpoint, claimUID, gpu.UUID) {
				klog.V(4).Infof("Fabric Manager: GPU %s still in use by other claims, skipping partition deactivation", gpu.UUID)
				return nil
			}
		}
	}
	partitionID, err := s.resolveFabricPartition(gpus)
	if err != nil {
		klog.Warningf("%v; skipping partition deactivation", err)
		return nil
	}
	// DeactivatePartition is idempotent: an already-inactive partition is a
	// no-op.
	klog.V(2).Infof("Fabric Manager: deactivating partition %d for %d-GPU claim %s/%s", partitionID, len(gpus), pc.Namespace, pc.Name)
	if err := s.fmManager.DeactivatePartition(partitionID); err != nil {
		return fmt.Errorf("deactivating fabric partition %d: %w", partitionID, err)
	}
	return nil
}

// attachFabricManagerPartitions populates the given GPU's size->partitionId
// mapping from its gpuModuleID using the FM Manager. It is a no-op when Fabric
// Manager partitioning is disabled. It gracefully skips GPUs whose module ID
// could not be resolved from NVML (e.g. a GPU that was already bound to
// vfio-pci at discovery time).
func (s *DeviceState) attachFabricManagerPartitions(gpu *GpuInfo) error {
	if !s.fabricManagerPartitioningEnabled() || gpu == nil {
		return nil
	}
	if gpu.gpuModuleID == 0 {
		klog.Warningf("GPU %s has no gpuModuleID; skipping Fabric Manager partition attributes. "+
			"This happens when the GPU was bound to vfio-pci before discovery (e.g. an active passthrough claim across a plugin restart).",
			gpu.CanonicalName())
		return nil
	}
	bySize, err := s.fmManager.GetPartitionsBySizeByModuleID(gpu.gpuModuleID)
	if err != nil {
		return fmt.Errorf("getting partition-by-size mapping for moduleID %d: %w", gpu.gpuModuleID, err)
	}
	gpu.partitionsBySize = bySize
	return nil
}

func (s *DeviceState) fabricManagerPartitioningEnabled() bool {
	return featuregates.Enabled(featuregates.FabricManagerPartitioning) && s.fmManager != nil
}

// activateFabricPartition activates the FM partition formed by the physical
// GPUs backing the claim's allocation results. The caller must ensure
// fabricManagerPartitioningEnabled() is true and that the claim is allocated.
func (s *DeviceState) activateFabricPartition(claim *resourceapi.ResourceClaim) error {
	if isAdminAccess(claim.Status.Allocation.Devices.Results) {
		return nil
	}

	gpus := s.gpuInfosFromPreparedClaim(claim.Status.Allocation.Devices.Results)
	if len(gpus) == 0 {
		return nil
	}
	partitionID, err := s.resolveFabricPartition(gpus)
	if err != nil {
		return fmt.Errorf("failed to resolve fabric partition activation: %w", err)
	}
	// ActivatePartition is idempotent: a retried Prepare (e.g. after a later
	// step failed) that hits an already-active partition is a no-op rather than
	// an FM in-use error.
	klog.V(2).Infof("Fabric Manager: activating partition %d for %d-GPU claim %s", partitionID, len(gpus), ResourceClaimToString(claim))
	if err := s.fmManager.ActivatePartition(partitionID); err != nil {
		return fmt.Errorf("activating fabric partition %d: %w", partitionID, err)
	}
	return nil
}

// GetOpaqueDeviceConfigs returns an ordered list of the configs contained in possibleConfigs for this driver.
//
// Configs can either come from the resource claim itself or from the device
// class associated with the request. Configs coming directly from the resource
// claim take precedence over configs coming from the device class. Moreover,
// configs found later in the list of configs attached to its source take
// precedence over configs found earlier in the list for that source.
//
// All of the configs relevant to the driver from the list of possibleConfigs
// will be returned in order of precedence (from lowest to highest). If no
// configs are found, nil is returned.
func GetOpaqueDeviceConfigs(
	decoder runtime.Decoder,
	driverName string,
	possibleConfigs []resourceapi.DeviceAllocationConfiguration,
) ([]*OpaqueDeviceConfig, error) {
	// Collect all configs in order of reverse precedence.
	var classConfigs []resourceapi.DeviceAllocationConfiguration
	var claimConfigs []resourceapi.DeviceAllocationConfiguration
	var candidateConfigs []resourceapi.DeviceAllocationConfiguration
	for _, config := range possibleConfigs {
		switch config.Source {
		case resourceapi.AllocationConfigSourceClass:
			classConfigs = append(classConfigs, config)
		case resourceapi.AllocationConfigSourceClaim:
			claimConfigs = append(claimConfigs, config)
		default:
			return nil, fmt.Errorf("invalid config source: %v", config.Source)
		}
	}
	candidateConfigs = append(candidateConfigs, classConfigs...)
	candidateConfigs = append(candidateConfigs, claimConfigs...)

	// Decode all configs that are relevant for the driver.
	var resultConfigs []*OpaqueDeviceConfig
	for _, config := range candidateConfigs {
		// If this is nil, the driver doesn't support some future API extension
		// and needs to be updated.
		if config.Opaque == nil {
			return nil, fmt.Errorf("only opaque parameters are supported by this driver")
		}

		// Configs for different drivers may have been specified because a
		// single request can be satisfied by different drivers. This is not
		// an error -- drivers must skip over other driver's configs in order
		// to support this.
		if config.Opaque.Driver != driverName {
			continue
		}

		decodedConfig, err := runtime.Decode(decoder, config.Opaque.Parameters.Raw)
		if err != nil {
			return nil, fmt.Errorf("error decoding config parameters: %w", err)
		}

		resultConfig := &OpaqueDeviceConfig{
			Requests: config.Requests,
			Config:   decodedConfig,
		}

		resultConfigs = append(resultConfigs, resultConfig)
	}

	return resultConfigs, nil
}

// requestedNonAdminDevices returns the set of device names requested by the claim,
// excluding admin-access allocations.
func (s *DeviceState) requestedNonAdminDevices(claim *resourceapi.ResourceClaim) map[string]struct{} {
	requested := make(map[string]struct{}, len(claim.Status.Allocation.Devices.Results))

	for _, r := range claim.Status.Allocation.Devices.Results {
		if r.Driver != DriverName {
			continue
		}
		if r.AdminAccess != nil && *r.AdminAccess {
			continue
		}
		requested[r.Device] = struct{}{}
	}
	return requested
}

// validateNoOverlappingPreparedDevices checks whether the given claim requests any device that is
// already allocated (non-admin) to a different claim that has completed preparation.
func (s *DeviceState) validateNoOverlappingPreparedDevices(checkpoint *Checkpoint, claim *resourceapi.ResourceClaim) error {
	claimUID := string(claim.UID)

	// Get the set of requested non-admin devices for the current claim.
	requestedDevices := s.requestedNonAdminDevices(claim)
	if len(requestedDevices) == 0 {
		return nil
	}

	for existingClaimUID, pc := range checkpoint.V2.PreparedClaims {
		// Skip the current claim.
		if existingClaimUID == claimUID {
			continue
		}
		if pc.CheckpointState != ClaimCheckpointStatePrepareCompleted {
			continue
		}

		// Get the non-admin devices from the prepared claim in the checkpoint.
		// We allow overlapping device allocations only if they are requested with admin access.
		preparedDevices := pc.GetNonAdminDevices()
		if len(preparedDevices) == 0 {
			continue
		}

		// Check for overlaps between requested devices from the current claim and others.
		for device := range requestedDevices {
			if _, found := preparedDevices[device]; found {
				if isConsumableSharesEnabled(s.config) {
					dev := s.perGPUAllocatable.GetAllocatableDevice(device)
					if dev != nil && dev.Type() != VfioDeviceType {
						existingConfig, err := s.getNormalizedDeviceConfig(pc.Status.Allocation, device)
						if err != nil {
							return fmt.Errorf("error resolving config for existing prepared claim %s on device %s: %w", existingClaimUID, device, err)
						}
						incomingConfig, err := s.getNormalizedDeviceConfig(claim.Status.Allocation, device)
						if err != nil {
							return fmt.Errorf("error resolving config for incoming claim %s on device %s: %w", claimUID, device, err)
						}
						if !reflect.DeepEqual(existingConfig, incomingConfig) {
							return fmt.Errorf("requested device %s has conflicting configuration with already prepared claim %s", device, existingClaimUID)
						}
						continue
					}
				}
				return fmt.Errorf(
					"requested device %s is already allocated to different claim %s",
					device, existingClaimUID,
				)
			}
		}
	}
	return nil
}

func (s *DeviceState) getNormalizedDeviceConfig(allocation *resourceapi.AllocationResult, deviceName string) (configapi.Interface, error) {
	configResultsMap, err := s.getConfigResultsMap(allocation)
	if err != nil {
		return nil, err
	}

	for configObj, results := range configResultsMap {
		for _, res := range results {
			if res.Device == deviceName {
				return normalizeAndValidateConfig(configObj)
			}
		}
	}

	return nil, nil
}

func (s *DeviceState) getConfigResultsMap(allocation *resourceapi.AllocationResult) (map[runtime.Object][]*resourceapi.DeviceRequestAllocationResult, error) {
	if allocation == nil {
		return nil, nil
	}

	configs, err := getDeviceConfigsWithDefaults(allocation.Devices.Config)
	if err != nil {
		return nil, err
	}

	// Look through the configs and figure out which one will be applied to
	// each device allocation result based on their order of precedence and type.
	configResultsMap := make(map[runtime.Object][]*resourceapi.DeviceRequestAllocationResult)
	for _, result := range allocation.Devices.Results {
		if result.Driver != DriverName {
			continue
		}
		device := s.perGPUAllocatable.GetAllocatableDevice(result.Device)
		if device == nil {
			return nil, fmt.Errorf("allocatable not found for device %q", result.Device)
		}
		for _, c := range slices.Backward(configs) {
			if slices.Contains(c.Requests, result.Request) {
				if err := validateDeviceConfigType(c.Config, device, &result); err != nil {
					return nil, err
				}
				configResultsMap[c.Config] = append(configResultsMap[c.Config], &result)
				break
			}
			if len(c.Requests) == 0 {
				if !matchesDeviceType(c.Config, device) {
					continue
				}
				configResultsMap[c.Config] = append(configResultsMap[c.Config], &result)
				break
			}
		}
	}

	return configResultsMap, nil
}

func getDeviceConfigsWithDefaults(rawConfigs []resourceapi.DeviceAllocationConfiguration) ([]*OpaqueDeviceConfig, error) {
	// Retrieve the full set of device configs for the driver.
	configs, err := GetOpaqueDeviceConfigs(
		configapi.StrictDecoder,
		DriverName,
		rawConfigs,
	)
	if err != nil {
		return nil, fmt.Errorf("error getting opaque device configs: %w", err)
	}

	// Add the default GPU and MIG device Configs to the front of the config
	// list with the lowest precedence. This guarantees there will be at least
	// one of each config in the list with len(Requests) == 0 for the lookup below.
	defaults := []*OpaqueDeviceConfig{
		{Requests: []string{}, Config: configapi.DefaultGpuConfig()},
		{Requests: []string{}, Config: configapi.DefaultMigDeviceConfig()},
	}
	if featuregates.Enabled(featuregates.PassthroughSupport) {
		defaults = append(defaults, &OpaqueDeviceConfig{
			Requests: []string{},
			Config:   configapi.DefaultVfioDeviceConfig(),
		})
	}

	return append(defaults, configs...), nil
}

func validateDeviceConfigType(c runtime.Object, dev *AllocatableDevice, result *resourceapi.DeviceRequestAllocationResult) error {
	switch c.(type) {
	case *configapi.GpuConfig:
		if dev.Type() != GpuDeviceType {
			return fmt.Errorf("cannot apply GpuConfig to device %q of type %q (request: %v)", result.Device, dev.Type(), result.Request)
		}
	case *configapi.MigDeviceConfig:
		if !dev.IsStaticOrDynMigDevice() {
			return fmt.Errorf("cannot apply MigDeviceConfig to device %q of type %q (request: %v)", result.Device, dev.Type(), result.Request)
		}
	case *configapi.VfioDeviceConfig:
		if dev.Type() != VfioDeviceType {
			return fmt.Errorf("cannot apply VfioDeviceConfig to device %q of type %q (request: %v)", result.Device, dev.Type(), result.Request)
		}
	}
	return nil
}

func matchesDeviceType(c runtime.Object, dev *AllocatableDevice) bool {
	switch c.(type) {
	case *configapi.GpuConfig:
		return dev.Type() == GpuDeviceType
	case *configapi.MigDeviceConfig:
		return dev.IsStaticOrDynMigDevice()
	case *configapi.VfioDeviceConfig:
		return dev.Type() == VfioDeviceType
	default:
		return false
	}
}

func normalizeAndValidateConfig(c runtime.Object) (configapi.Interface, error) {
	cloned := c.DeepCopyObject()
	var config configapi.Interface
	switch castConfig := cloned.(type) {
	case *configapi.GpuConfig:
		config = castConfig
	case *configapi.MigDeviceConfig:
		config = castConfig
	case *configapi.VfioDeviceConfig:
		config = castConfig
	default:
		return nil, fmt.Errorf("runtime object is not a recognized configuration: %T", castConfig)
	}

	if err := config.Normalize(); err != nil {
		return nil, fmt.Errorf("error normalizing config: %w", err)
	}
	if err := config.Validate(); err != nil {
		return nil, fmt.Errorf("error validating config: %w", err)
	}
	return config, nil
}

func (s *DeviceState) getPreparedMigDevice(checkpoint *Checkpoint, deviceName string) *PreparedMigDevice {
	if checkpoint == nil || checkpoint.V2 == nil {
		return nil
	}
	for _, pc := range checkpoint.V2.PreparedClaims {
		if pc.CheckpointState != ClaimCheckpointStatePrepareCompleted {
			continue
		}
		for _, group := range pc.PreparedDevices {
			for _, dev := range group.Devices {
				if dev.Type() == PreparedMigDeviceType && dev.Mig != nil && dev.Mig.Device != nil && dev.Mig.Device.DeviceName == deviceName {
					return dev.Mig
				}
			}
		}
	}
	return nil
}

func isMigDeviceInUseByOtherClaims(checkpoint *Checkpoint, claimUID string, migUUID string, deviceName string) bool {
	if checkpoint == nil || checkpoint.V2 == nil {
		return false
	}
	for otherUID, otherClaim := range checkpoint.V2.PreparedClaims {
		if otherUID == claimUID {
			continue
		}
		if otherClaim.CheckpointState != ClaimCheckpointStatePrepareCompleted {
			continue
		}
		for _, devName := range otherClaim.PreparedDevices.GetDeviceNames() {
			if devName == deviceName {
				return true
			}
		}
		for _, u := range otherClaim.PreparedDevices.MigDeviceUUIDs() {
			if u == migUUID {
				return true
			}
		}
	}
	return false
}

func isGpuUUIDInUseByOtherClaims(checkpoint *Checkpoint, claimUID string, gpuUUID string) bool {
	if checkpoint == nil || checkpoint.V2 == nil {
		return false
	}
	for otherUID, otherClaim := range checkpoint.V2.PreparedClaims {
		if otherUID == claimUID {
			continue
		}
		if otherClaim.CheckpointState != ClaimCheckpointStatePrepareCompleted {
			continue
		}
		for _, group := range otherClaim.PreparedDevices {
			for _, dev := range group.Devices {
				if dev.Gpu == nil || dev.Gpu.Info == nil || dev.Gpu.Device == nil {
					continue
				}
				if preparedClaimDeviceHasAdminAccess(&otherClaim, dev.Gpu.Device.DeviceName) {
					continue
				}
				if dev.Gpu.Info.UUID == gpuUUID {
					return true
				}
			}
		}
	}
	return false
}

func preparedClaimDeviceHasAdminAccess(claim *PreparedClaim, deviceName DeviceName) bool {
	if claim == nil || claim.Status.Allocation == nil {
		return false
	}
	for _, result := range claim.Status.Allocation.Devices.Results {
		if result.Driver != DriverName || result.Device != deviceName {
			continue
		}
		if result.AdminAccess != nil && *result.AdminAccess {
			return true
		}
	}
	return false
}

// Make this best-effort for now (do not return an error, but log details).
func (s *DeviceState) deleteMigDevIfExistsAndNotUsedByCompletedClaim(ms *MigSpecTuple, dname DeviceName, completelyPreparedClaims PreparedClaimsByUID) error {
	for uid, claim := range completelyPreparedClaims {
		for _, res := range claim.Status.Allocation.Devices.Results {
			if res.Device == dname {
				klog.V(1).Infof("Device %s is in use by completely prepared claim %s", dname, PreparedClaimToString(&claim, uid))
				return nil
			}
		}
	}

	klog.V(1).Infof("Device '%s' is not in use by any completely prepared claim, find corresponding actual MIG device", dname)
	mlt, err := s.nvdevlib.FindMigDevBySpec(ms)
	if err != nil {
		return fmt.Errorf("failed to find MIG device %s (FindMigDevBySpec): %w", dname, err)
	}
	if mlt == nil {
		klog.V(1).Infof("No live MIG device corresponding to name %s currently exists (nothing to clean up)", dname)
		return nil
	}

	klog.V(1).Infof("MIG device corresponding to name %s found with UUID %s -- attempt to tear down", dname, mlt.MigUUID)
	if err := s.nvdevlib.deleteMigDevice(mlt); err != nil {
		return fmt.Errorf("failed to delete MIG device %s (deleteMigDevice): %w", dname, err)
	}

	return nil
}

func syncPreparedDevicesGaugeFromCheckpoint(nodeName string, cp *Checkpoint) {
	counts := make(map[string]int) // map of device type to count of devices of that type
	if cp == nil {
		return
	}
	lv := cp.ToLatestVersion()
	if lv != nil && lv.V2 != nil {
		for _, pc := range lv.V2.PreparedClaims {
			if pc.CheckpointState != ClaimCheckpointStatePrepareCompleted {
				continue
			}
			for _, g := range pc.PreparedDevices {
				for _, dev := range g.Devices {
					if _, ok := counts[dev.Type()]; !ok {
						counts[dev.Type()] = 0
					}
					counts[dev.Type()]++
				}
			}
		}
	}

	for _, dt := range []string{GpuDeviceType, PreparedMigDeviceType, VfioDeviceType, UnknownDeviceType} {
		if count, ok := counts[dt]; !ok {
			drametrics.SetPreparedDevicesCounts(nodeName, DriverName, dt, 0)
		} else {
			drametrics.SetPreparedDevicesCounts(nodeName, DriverName, dt, count)
		}
	}
}

// AddDeviceTaint adds or updates a DRA device taint on the given device under
// the DeviceState lock. Returns true if the taint set was actually modified.
func (s *DeviceState) AddDeviceTaint(d *AllocatableDevice, taint *resourceapi.DeviceTaint) bool {
	s.Lock()
	defer s.Unlock()
	return d.AddOrUpdateTaint(taint)
}

// Returns false on nodes where GPU hardware is not MIG capable (L4/Ada,T4/Turing)
// Used by the driver at startup to gate the DynamicMIG-specific code paths.
func (s *DeviceState) IsMigCapable() bool {
	for _, gpu := range s.nvdevlib.gpuInfosByUUID {
		if gpu.migCapable {
			return true
		}
	}
	return false
}

func isAdminAccess(results []resourceapi.DeviceRequestAllocationResult) bool {
	for _, r := range results {
		if r.Driver != DriverName {
			continue
		}
		if r.AdminAccess != nil && *r.AdminAccess {
			return true
		}
	}
	return false
}

// dynamicMIGParentDevice returns the allocatable full-GPU device of a Dynamic
// MIG device, or nil when the full GPU is not allocatable.
func (s *DeviceState) dynamicMIGParentDevice(device *AllocatableDevice) *AllocatableDevice {
	if device.MigDynamic == nil || device.MigDynamic.Parent == nil {
		return nil
	}
	return s.perGPUAllocatable.GetGPUDeviceByPCIBusID(device.MigDynamic.Parent.pciBusID)
}

// clearDynamicMIGXIDTaint clears health state that belongs to a concrete
// Dynamic MIG incarnation after that device no longer exists. Parent-scoped
// taints, such as GPU lost or an unavailable health monitor, remain intact,
// and so does an XID taint that the parent GPU still carries as critical: a
// GPU-scoped XID is fanned out to every MIG device on the GPU, and destroying
// one MIG instance does not fix the GPU. (On GPUs whose full-GPU device is
// not allocatable, such as Ampere in MIG mode, the parent's taints are not
// tracked and the XID taint is cleared.)
//
// NOTE: An XID event already queued before deletion could re-add the taint.
// This cleanup intentionally does not track concrete MIG
// incarnations; that race can be addressed separately if observed.
func (s *DeviceState) clearDynamicMIGXIDTaint(name DeviceName) bool {
	device := s.perGPUAllocatable.GetAllocatableDevice(name)
	if device == nil || device.Type() != MigDynamicDeviceType {
		return false
	}
	if parent := s.dynamicMIGParentDevice(device); parent != nil {
		for _, taint := range parent.Taints() {
			if taint.Key == TaintKeyXID && taint.Effect != resourceapi.DeviceTaintEffectNone {
				klog.V(4).Infof("Keeping health taint on destroyed Dynamic MIG device %s: parent GPU %s still has critical XID %s",
					name, parent.CanonicalName(), taint.Value)
				return false
			}
		}
	}
	if taint, removed := device.RemoveTaint(TaintKeyXID); removed {
		klog.V(4).Infof("Cleared health taint for destroyed Dynamic MIG device %s: key=%q, value=%q, effect=%s",
			name, taint.Key, taint.Value, taint.Effect)
		return true
	}
	return false
}
