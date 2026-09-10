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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/pmezard/go-difflib/difflib"
	resourceapi "k8s.io/api/resource/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/version"
	"k8s.io/dynamic-resource-allocation/kubeletplugin"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/kubelet/checkpointmanager"
	cperrors "k8s.io/kubernetes/pkg/kubelet/checkpointmanager/errors"
	cdiapi "tags.cncf.io/container-device-interface/pkg/cdi"

	configapi "sigs.k8s.io/dra-driver-nvidia-gpu/api/nvidia.com/resource/v1beta1"
	"sigs.k8s.io/dra-driver-nvidia-gpu/internal/common"
	"sigs.k8s.io/dra-driver-nvidia-gpu/internal/lookup/root"
	"sigs.k8s.io/dra-driver-nvidia-gpu/pkg/bootid"
	"sigs.k8s.io/dra-driver-nvidia-gpu/pkg/featuregates"
	drametrics "sigs.k8s.io/dra-driver-nvidia-gpu/pkg/metrics"
)

type OpaqueDeviceConfig struct {
	Requests []string
	Config   runtime.Object
}

type DeviceConfigState struct {
	Type           string
	ComputeDomain  string
	containerEdits *cdiapi.ContainerEdits
}

type DeviceState struct {
	sync.Mutex
	cdi                      *CDIHandler
	computeDomainManager     *ComputeDomainManager
	checkpointCleanupManager *CheckpointCleanupManager
	allocatable              AllocatableDevices
	config                   *Config
	nvdevlib                 *deviceLib

	checkpointManager checkpointmanager.CheckpointManager
}

func NewDeviceState(ctx context.Context, config *Config) (*DeviceState, error) {
	driver := root.New(root.WithDriverRoot(config.flags.containerDriverRoot))
	nvdevlib, err := newDeviceLib(driver)
	if err != nil {
		return nil, fmt.Errorf("failed to create device library: %w", err)
	}

	// Check driver version if IMEXDaemonsWithDNSNames feature gate is
	// enabled. DNS-named IMEX daemons are a driver-managed-only concept (the
	// driver never creates IMEX daemons under host-managed IMEX), so this
	// check does not apply there
	if !config.imexConfig.EffectiveHostManaged() && featuregates.Enabled(featuregates.IMEXDaemonsWithDNSNames) {
		if err := validateDriverVersionForIMEXDaemonsWithDNSNames(config.flags, nvdevlib); err != nil {
			return nil, fmt.Errorf("driver version validation failed: %w", err)
		}
	}

	allocatable, err := nvdevlib.enumerateAllPossibleDevices(config)
	if err != nil {
		return nil, fmt.Errorf("error enumerating all possible devices: %w", err)
	}

	devRoot := driver.DevRoot
	klog.Infof("using devRoot=%v", devRoot)

	hostDriverRoot := config.flags.hostDriverRoot
	cdi, err := NewCDIHandler(
		WithNvml(nvdevlib.nvmllib),
		WithDeviceLib(nvdevlib),
		WithDriverRoot(driver.Root),
		WithDevRoot(devRoot),
		WithTargetDriverRoot(hostDriverRoot),
		WithNVIDIACDIHookPath(config.flags.nvidiaCDIHookPath),
		WithCDIRoot(config.flags.cdiRoot),
		WithVendor(cdiVendor),
	)
	if err != nil {
		return nil, fmt.Errorf("unable to create CDI handler: %w", err)
	}

	computeDomainManager, err := NewComputeDomainManager(config, nvdevlib.getCliqueID)
	if err != nil {
		return nil, fmt.Errorf("failed to create ComputeDomain manager: %w", err)
	}

	if err := cdi.CreateStandardDeviceSpecFile(allocatable); err != nil {
		return nil, fmt.Errorf("unable to create base CDI spec file: %w", err)
	}

	checkpointManager, err := checkpointmanager.NewCheckpointManager(config.DriverPluginPath())
	if err != nil {
		return nil, fmt.Errorf("unable to create checkpoint manager: %w", err)
	}

	state := &DeviceState{
		cdi:                  cdi,
		computeDomainManager: computeDomainManager,
		allocatable:          allocatable,
		config:               config,
		nvdevlib:             nvdevlib,
		checkpointManager:    checkpointManager,
	}
	state.checkpointCleanupManager = NewCheckpointCleanupManager(state, config.clientsets.Resource)

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
			klog.Infof("Found previous checkpoint: %s", c)
			cp, err := state.getCheckpoint()
			if err != nil {
				return nil, fmt.Errorf("unable to get checkpoint: %w", err)
			}
			storedBootID := cp.GetNodeBootID()
			if storedBootID == "" { //nolint:gocritic,staticcheck
				// legacy checkpoint file does not contain a boot ID, inject current boot ID
				// note: this is a temporary workaround to ensure that the checkpoint file is always updated with the current boot ID
				// note: this will temporary break the assertion that its prepared devices are prepared by the same boot ID
				klog.V(4).Info("The existing checkpoint file does not contain a boot ID, injecting current boot ID")
				err := state.updateCheckpoint(func(checkpoint *Checkpoint) {
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
	if err := state.createCheckpoint(newCheckpoint); err != nil {
		return nil, fmt.Errorf("unable to create checkpoint: %w", err)
	}

	return state, nil
}

func (s *DeviceState) Prepare(ctx context.Context, claim *resourceapi.ResourceClaim) ([]kubeletplugin.Device, error) {
	s.Lock()
	defer s.Unlock()

	claimUID := string(claim.UID)

	checkpoint, err := s.getCheckpoint()
	if err != nil {
		return nil, fmt.Errorf("unable to get checkpoint: %w", err)
	}

	preparedClaim, exists := checkpoint.V2.PreparedClaims[claimUID]
	if exists && preparedClaim.CheckpointState == ClaimCheckpointStatePrepareCompleted {
		// Associated device(s) has/ave been prepared by us. Prepare() must be
		// idempotent, as it may be invoked more than once per claim (and actual
		// device preparation must happen at most once).
		klog.V(2).Infof("Skip prepare: claim already in PrepareCompleted state: %s", ResourceClaimToString(claim))
		return preparedClaim.PreparedDevices.GetDevices(), nil
	}
	if exists && preparedClaim.CheckpointState == ClaimCheckpointStatePrepareAborted && claimMatchesPreparedClaim(preparedClaim, claim) {
		return nil, permanentError{fmt.Errorf("stale prepare for claim %s: claim prepare was already aborted", ResourceClaimToString(claim))}
	}

	// In certain scenarios, the same device can be prepared/allocated more than once for different claims
	// due to races between data processing in different goroutines in the scheduler, or when pods are
	// force-deleted while the kubelet still considers the devices allocated.
	// To prevent this, we check whether any device requested in the incoming claim has already been prepared
	// and fail the request if so (unless the prior preparation was performed with admin access).
	// More details: https://github.com/kubernetes/kubernetes/pull/136269
	if err := s.validateNoOverlappingPreparedDevices(checkpoint, claim); err != nil {
		return nil, fmt.Errorf("unable to prepare claim %v: %w", claimUID, err)
	}

	err = s.updateCheckpoint(func(checkpoint *Checkpoint) {
		checkpoint.V2.PreparedClaims[claimUID] = PreparedClaim{
			CheckpointState: ClaimCheckpointStatePrepareStarted,
			Status:          claim.Status,
			Name:            claim.Name,
			Namespace:       claim.Namespace,
		}
	})
	if err != nil {
		return nil, fmt.Errorf("unable to update checkpoint: %w", err)
	}
	klog.V(6).Infof("checkpoint updated for claim %v", claimUID)

	preparedDevices, err := s.prepareDevices(ctx, claim)
	if err != nil {
		return nil, fmt.Errorf("prepare devices failed: %w", err)
	}

	if err := s.cdi.CreateClaimSpecFile(claimUID, preparedDevices); err != nil {
		return nil, fmt.Errorf("unable to create CDI spec file for claim: %w", err)
	}

	// Some errors above along the Prepare() path leave the claim in the
	// checkpoint, in the 'PrepareStarted' state. That's deliberate, to annotate
	// potentially partially prepared claims. There is an asynchronous
	// checkpoint cleanup procedure that identifies when such entry goes stale.
	err = s.updateCheckpoint(func(checkpoint *Checkpoint) {
		checkpoint.V2.PreparedClaims[claimUID] = PreparedClaim{
			CheckpointState: ClaimCheckpointStatePrepareCompleted,
			Status:          claim.Status,
			PreparedDevices: preparedDevices,
			Name:            claim.Name,
			Namespace:       claim.Namespace,
		}
	})
	if err != nil {
		return nil, fmt.Errorf("unable to update checkpoint: %w", err)
	}
	klog.V(6).Infof("checkpoint updated for claim %v", claimUID)

	return preparedDevices.GetDevices(), nil
}

func (s *DeviceState) Unprepare(ctx context.Context, claimRef kubeletplugin.NamespacedObject) error {
	s.Lock()
	defer s.Unlock()

	klog.V(6).Infof("Unprepare() for claim '%s'", claimRef.String())

	// Rely on local checkpoint state for ability to clean up.
	checkpoint, err := s.getCheckpoint()
	if err != nil {
		return fmt.Errorf("unable to get checkpoint: %w", err)
	}

	claimUID := string(claimRef.UID)
	pc, exists := checkpoint.V2.PreparedClaims[claimUID]
	if !exists {
		// Not an error: if this claim UID is not in the checkpoint then this
		// device was never prepared or has already been unprepared (assume that
		// Prepare+Checkpoint are done transactionally). Note that
		// claimRef.String() contains namespace, name, UID.
		klog.V(2).Infof("Unprepare noop: claim not found in checkpoint data: %v", claimRef.String())
		return nil
	}

	// If pc.Status.Allocation is 'nil', attempt to pull the status from the API
	// server. This should only ever happen if we have unmarshaled from a legacy
	// checkpoint format that did not include the Status field.
	//
	// TODO: Remove this one release cycle following the v25.3.0 release
	if pc.Status.Allocation == nil {
		klog.Infof("PreparedClaim Status not set in Checkpoint for claim '%s': attempting to pull it from API server", claimRef.String())
		claim, err := s.config.clientsets.Resource.ResourceClaims(claimRef.Namespace).Get(
			ctx,
			claimRef.Name,
			metav1.GetOptions{})

		// TODO: distinguish errors -- if this is a 'not found' error then this
		// is permanent and we may want to drop the claim from the checkpoint.
		// Otherwise, this might be worth retrying?
		if err != nil {
			return permanentError{fmt.Errorf("failed to fetch ResourceClaim %s: %w", claimRef.String(), err)}
		}
		if claim.Status.Allocation == nil {
			// TODO: drop claim from checkpoint?
			return permanentError{fmt.Errorf("no allocation set in ResourceClaim %s", claim.String())}
		}
		pc.Status = claim.Status
	}

	switch pc.CheckpointState {
	case ClaimCheckpointStatePrepareCompleted:
		if err := s.unprepareDevices(ctx, &pc.Status); err != nil {
			return fmt.Errorf("unprepare devices failed: %w", err)
		}
		if err := s.deleteClaimFromCheckpoint(claimRef); err != nil {
			return fmt.Errorf("error deleting completed claim from checkpoint: %w", err)
		}
	case ClaimCheckpointStatePrepareStarted:
		if err := s.unprepareDevices(ctx, &pc.Status); err != nil {
			return fmt.Errorf("unprepare devices failed: %w", err)
		}
		// Keep a short-lived PrepareAborted entry so stale prepare retries for the
		// same claim version cannot recreate device state after unprepare.
		if err := s.markClaimPrepareAbortedInCheckpoint(claimRef, pc); err != nil {
			return fmt.Errorf("error marking claim PrepareAborted in checkpoint: %w", err)
		}
	case ClaimCheckpointStatePrepareAborted:
		klog.V(2).Infof("Unprepare noop: claim in PrepareAborted state: %v", claimRef.String())
		return nil
	default:
		return fmt.Errorf("unsupported ClaimCheckpointState: %v", pc.CheckpointState)
	}

	// Assume that this is a retryable error. If there is any chance for this
	// error to be permanent: drop the claim from checkpoint then regardless?
	if err := s.cdi.DeleteClaimSpecFileIfExists(claimUID); err != nil {
		return fmt.Errorf("unable to delete CDI spec file for claim: %w", err)
	}
	return nil
}

func claimMatchesPreparedClaim(pc PreparedClaim, claim *resourceapi.ResourceClaim) bool {
	return apiequality.Semantic.DeepEqual(pc.Status, claim.Status)
}

func (s *DeviceState) createCheckpoint(cp *Checkpoint) error {
	if err := s.checkpointManager.CreateCheckpoint(DriverPluginCheckpointFileBasename, cp); err != nil {
		return err
	}
	syncPreparedDevicesGaugeFromCheckpoint(s.config.flags.nodeName, cp)
	return nil
}

func (s *DeviceState) getCheckpoint() (*Checkpoint, error) {
	checkpoint := &Checkpoint{}
	if err := s.checkpointManager.GetCheckpoint(DriverPluginCheckpointFileBasename, checkpoint); err != nil {
		if errors.Is(err, cperrors.CorruptCheckpointError{}) {
			logCheckpointDiff(s.config.DriverPluginPath(), checkpoint)
		}
		return nil, err
	}
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
// mutation of the checkpoint must go through this function. The
// read-mutate-write sequence must be performed under a lock: we must be
// conceptually certain that multiple read-mutate-write actions never overlap.
// Currently, it is assumed that any caller gets here by first acquiring
// driver's `pulock`.
func (s *DeviceState) updateCheckpoint(mutate func(*Checkpoint)) error {
	checkpoint, err := s.getCheckpoint()
	if err != nil {
		return fmt.Errorf("unable to get checkpoint: %w", err)
	}

	mutate(checkpoint)

	if err := s.checkpointManager.CreateCheckpoint(DriverPluginCheckpointFileBasename, checkpoint); err != nil {
		return fmt.Errorf("unable to create checkpoint: %w", err)
	}

	syncPreparedDevicesGaugeFromCheckpoint(s.config.flags.nodeName, checkpoint)
	return nil
}

func (s *DeviceState) deleteClaimFromCheckpoint(claimRef kubeletplugin.NamespacedObject) error {
	err := s.updateCheckpoint(func(cp *Checkpoint) {
		delete(cp.V2.PreparedClaims, string(claimRef.UID))
	})
	if err != nil {
		return fmt.Errorf("unable to update checkpoint: %w", err)
	}
	klog.V(6).Infof("Deleted claim from checkpoint: %s", claimRef.String())
	return nil
}

func (s *DeviceState) markClaimPrepareAbortedInCheckpoint(claimRef kubeletplugin.NamespacedObject, pc PreparedClaim) error {
	abortedAt := metav1.Now()
	pc.CheckpointState = ClaimCheckpointStatePrepareAborted
	pc.PreparedDevices = nil
	pc.Name = claimRef.Name
	pc.Namespace = claimRef.Namespace
	pc.AbortedAt = &abortedAt

	err := s.updateCheckpoint(func(cp *Checkpoint) {
		cp.V2.PreparedClaims[string(claimRef.UID)] = pc
	})
	if err != nil {
		return fmt.Errorf("unable to update checkpoint: %w", err)
	}
	klog.V(6).Infof("Marked claim PrepareAborted in checkpoint: %s", claimRef.String())
	return nil
}

func (s *DeviceState) deleteExpiredPrepareAbortedClaimsFromCheckpoint(now time.Time, ttl time.Duration) (int, error) {
	s.Lock()
	defer s.Unlock()

	checkpoint, err := s.getCheckpoint()
	if err != nil {
		return 0, fmt.Errorf("unable to get checkpoint: %w", err)
	}

	expired := expiredPrepareAbortedClaimEntries(checkpoint, now, ttl)
	if len(expired) == 0 {
		return 0, nil
	}

	err = s.updateCheckpoint(func(cp *Checkpoint) {
		for _, uid := range expired {
			delete(cp.V2.PreparedClaims, uid)
		}
	})
	if err != nil {
		return 0, fmt.Errorf("unable to update checkpoint: %w", err)
	}
	return len(expired), nil
}

func expiredPrepareAbortedClaimEntries(cp *Checkpoint, now time.Time, ttl time.Duration) []string {
	var expired []string
	if cp == nil || cp.V2 == nil {
		return expired
	}
	for uid, claim := range cp.V2.PreparedClaims {
		if claim.CheckpointState != ClaimCheckpointStatePrepareAborted {
			continue
		}
		if claim.AbortedAt == nil || now.Sub(claim.AbortedAt.Time) >= ttl {
			expired = append(expired, uid)
		}
	}
	slices.Sort(expired)
	return expired
}

func (s *DeviceState) prepareDevices(ctx context.Context, claim *resourceapi.ResourceClaim) (PreparedDevices, error) {
	// Generate a mapping of each OpaqueDeviceConfigs to the Device.Results it
	// applies to. Strict-decode: data is provided by user and may be completely
	// unvalidated so far (in absence of validating webhook).
	configResultsMap, err := s.getConfigResultsMap(&claim.Status, configapi.StrictDecoder)
	if err != nil {
		return nil, fmt.Errorf("error generating configResultsMap: %w", err)
	}

	// Normalize, validate, and apply all configs associated with devices that
	// need to be prepared. Track device group configs generated from applying the
	// config to the set of device allocation results.
	preparedDeviceGroupConfigState := make(map[runtime.Object]*DeviceConfigState)
	for c, results := range configResultsMap {
		// Cast the opaque config to a configapi.Interface type
		var config configapi.Interface
		switch castConfig := c.(type) {
		case *configapi.ComputeDomainChannelConfig:
			config = castConfig
		case *configapi.ComputeDomainDaemonConfig:
			config = castConfig
		default:
			return nil, fmt.Errorf("runtime object is not a recognized configuration")
		}

		// Normalize the config to set any implied defaults.
		if err := config.Normalize(); err != nil {
			return nil, fmt.Errorf("error normalizing config: %w", err)
		}

		// Validate the config to ensure its integrity.
		if err := config.Validate(); err != nil {
			return nil, fmt.Errorf("error validating config: %w", err)
		}

		// Apply the config to the list of results associated with it.
		configState, err := s.applyConfig(ctx, config, claim, results)
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
			if d := s.cdi.GetStandardDevice(s.allocatable[result.Device]); d != "" {
				cdiDevices = append(cdiDevices, d)
			}
			if d := s.cdi.GetClaimDevice(string(claim.UID), s.allocatable[result.Device], preparedDeviceGroupConfigState[c].containerEdits); d != "" {
				cdiDevices = append(cdiDevices, d)
			}

			device := &CheckpointedDevice{
				Requests:     []string{result.Request},
				PoolName:     result.Pool,
				DeviceName:   result.Device,
				CDIDeviceIDs: cdiDevices,
			}

			var preparedDevice PreparedDevice
			switch s.allocatable[result.Device].Type() {
			case ComputeDomainChannelType:
				preparedDevice.Channel = &PreparedComputeDomainChannel{
					Info:   s.allocatable[result.Device].Channel,
					Device: device,
				}
			case ComputeDomainDaemonType:
				preparedDevice.Daemon = &PreparedComputeDomainDaemon{
					Info:   s.allocatable[result.Device].Daemon,
					Device: device,
				}
			}

			preparedDeviceGroup.Devices = append(preparedDeviceGroup.Devices, preparedDevice)
		}

		preparedDevices = append(preparedDevices, &preparedDeviceGroup)
	}
	return preparedDevices, nil
}

func (s *DeviceState) unprepareDevices(ctx context.Context, cs *resourceapi.ResourceClaimStatus) error {
	// Generate a mapping of each OpaqueDeviceConfigs to the Device.Results it
	// applies to. Non-strict decoding: do not error out on unknown fields (data
	// source is checkpointed JSON written by potentially newer versions of this
	// driver).
	configResultsMap, err := s.getConfigResultsMap(cs, configapi.NonstrictDecoder)
	if err != nil {
		return fmt.Errorf("error generating configResultsMap: %w", err)
	}

	// Unprepare any ComputeDomain daemons prepared for each group of prepared devices.
	for c := range configResultsMap {
		switch config := c.(type) {
		case *configapi.ComputeDomainChannelConfig:
			// Host-managed mode never adds the ComputeDomain node label, so
			// there is nothing to remove here.
			if s.config.imexConfig.EffectiveHostManaged() {
				continue
			}
			// If a channel type, remove the ComputeDomain label from the node
			if err := s.computeDomainManager.RemoveNodeLabel(ctx, config.DomainID); err != nil {
				return fmt.Errorf("error removing Node label for ComputeDomain: %w", err)
			}
		case *configapi.ComputeDomainDaemonConfig:
			// If a daemon type, unprepare the new ComputeDomain daemon.
			computeDomainDaemonSettings, err := s.computeDomainManager.NewSettings(config.DomainID)
			if err != nil {
				return fmt.Errorf("error creating ComputeDomain daemon settings: %w", err)
			}
			if err := computeDomainDaemonSettings.Unprepare(ctx); err != nil {
				return fmt.Errorf("error unpreparing ComputeDomain daemon settings: %w", err)
			}
		}
	}

	return nil
}

func (s *DeviceState) applyConfig(ctx context.Context, config configapi.Interface, claim *resourceapi.ResourceClaim, results []*resourceapi.DeviceRequestAllocationResult) (*DeviceConfigState, error) {
	switch castConfig := config.(type) {
	case *configapi.ComputeDomainChannelConfig:
		return s.applyComputeDomainChannelConfig(ctx, castConfig, claim, results)
	case *configapi.ComputeDomainDaemonConfig:
		return s.applyComputeDomainDaemonConfig(ctx, castConfig, claim, results)
	default:
		return nil, fmt.Errorf("unknown config type: %T", castConfig)
	}
}

func (s *DeviceState) applyComputeDomainChannelConfig(ctx context.Context, config *configapi.ComputeDomainChannelConfig, claim *resourceapi.ResourceClaim, results []*resourceapi.DeviceRequestAllocationResult) (*DeviceConfigState, error) {
	// Not an expected error, but a violated invariant.
	if len(results) != 1 {
		return nil, fmt.Errorf("applyComputeDomainChannelConfig: unexpected results %v", results)
	}

	// Host-managed IMEX skips daemon bootstrap and readiness bookkeeping entirely,
	// so the two modes are handled by separate functions instead of threading
	// hostManaged checks through a shared code path.
	if s.config.imexConfig.EffectiveHostManaged() {
		return s.applyComputeDomainChannelConfigHostManaged(ctx, config, claim, results[0])
	}
	return s.applyComputeDomainChannelConfigDriverManaged(ctx, config, claim)
}

// applyComputeDomainChannelConfigHostManaged handles a channel claim when the
// cluster admin (not the driver) owns the host nvidia-imex daemon. It skips
// the driver-managed IMEX daemon management entirely, but on fabric nodes
// still validates that the operator's host nvidia-imex daemon is actually
// ready before handing out a channel, via checkHostIMEXReady.
func (s *DeviceState) applyComputeDomainChannelConfigHostManaged(ctx context.Context, config *configapi.ComputeDomainChannelConfig, claim *resourceapi.ResourceClaim, result *resourceapi.DeviceRequestAllocationResult) (*DeviceConfigState, error) {
	device, ok := s.allocatable[result.Device]
	if !ok || device.Channel == nil {
		return nil, fmt.Errorf("applyComputeDomainChannelConfigHostManaged: %q is not an allocatable IMEX channel device", result.Device)
	}
	channelID := device.Channel.ID

	if err := s.assertImexChannelNotAllocated(channelID); err != nil {
		return nil, fmt.Errorf("allocation failed: %w", err)
	}

	if err := s.computeDomainManager.AssertComputeDomainNamespace(ctx, claim.Namespace, config.DomainID); err != nil {
		return nil, permanentError{fmt.Errorf("error asserting ComputeDomain's namespace: %w", err)}
	}

	configState := DeviceConfigState{
		Type:          ComputeDomainChannelType,
		ComputeDomain: config.DomainID,
	}

	if s.computeDomainManager.CliqueID() == "" {
		// Non-fabric node (e.g. not part of an MNNVL clique): do not inject
		// IMEX channel device nodes, but let the claim succeed.
		return &configState, nil
	}

	if err := s.nvdevlib.checkHostIMEXReady(s.config.flags.imexHostSocketPath); err != nil {
		return nil, fmt.Errorf("host nvidia-imex daemon readiness check failed: %w", err)
	}

	if channelID < 0 || channelID >= len(s.nvdevlib.nvCapImexChanDevInfos) {
		return nil, fmt.Errorf("applyComputeDomainChannelConfigHostManaged: channel %d out of range (node has %d IMEX channels)",
			channelID, len(s.nvdevlib.nvCapImexChanDevInfos))
	}
	edits := s.computeDomainManager.GetComputeDomainChannelContainerEdits(s.cdi.devRoot, s.nvdevlib.nvCapImexChanDevInfos[channelID])
	configState.containerEdits = configState.containerEdits.Append(edits)

	return &configState, nil
}

// applyComputeDomainChannelConfigDriverManaged handles a channel claim under
// the default, driver-managed model where it adds necessary node labels and checks
// the the IMEX daemon readiness.
func (s *DeviceState) applyComputeDomainChannelConfigDriverManaged(ctx context.Context, config *configapi.ComputeDomainChannelConfig, claim *resourceapi.ResourceClaim) (*DeviceConfigState, error) {
	// If explicitly requested, inject all channels instead of just one.
	chancount := 1
	if config.AllocationMode == configapi.ComputeDomainChannelAllocationModeAll {
		chancount = s.nvdevlib.maxImexChannelCount
	}

	// Declare a device group state object to populate.
	configState := DeviceConfigState{
		Type:          ComputeDomainChannelType,
		ComputeDomain: config.DomainID,
	}

	// Treat each request as a request for channel zero, even if
	// AllocationModeAll.
	if err := s.assertImexChannelNotAllocated(0); err != nil {
		return nil, fmt.Errorf("allocation failed: %w", err)
	}

	// Create any necessary ComputeDomain channels and gather their CDI container edits.
	if err := s.computeDomainManager.AssertComputeDomainNamespace(ctx, claim.Namespace, config.DomainID); err != nil {
		return nil, permanentError{fmt.Errorf("error asserting ComputeDomain's namespace: %w", err)}
	}

	if err := s.computeDomainManager.AddNodeLabel(ctx, config.DomainID); err != nil {
		return nil, fmt.Errorf("error adding Node label for ComputeDomain: %w", err)
	}

	if err := s.computeDomainManager.AssertComputeDomainReady(ctx, config.DomainID); err != nil {
		return nil, fmt.Errorf("error asserting ComputeDomain Ready: %w", err)
	}

	if s.computeDomainManager.CliqueID() == "" {
		// Do not inject IMEX channel device nodes.
		return &configState, nil
	}

	for _, info := range s.nvdevlib.nvCapImexChanDevInfos[:chancount] {
		edits := s.computeDomainManager.GetComputeDomainChannelContainerEdits(s.cdi.devRoot, info)
		configState.containerEdits = configState.containerEdits.Append(edits)
	}

	return &configState, nil
}

func (s *DeviceState) applyComputeDomainDaemonConfig(ctx context.Context, config *configapi.ComputeDomainDaemonConfig, claim *resourceapi.ResourceClaim, results []*resourceapi.DeviceRequestAllocationResult) (*DeviceConfigState, error) {
	// Host-managed IMEX runs no driver-managed ComputeDomain daemons, so
	// daemon devices are never published or allocated. A daemon claim
	// reaching Prepare means a stale or hand-crafted object; reject it
	// permanently.
	if s.config.imexConfig.EffectiveHostManaged() {
		return nil, permanentError{fmt.Errorf("ComputeDomain daemon claims are not supported when imex.mode=hostManaged")}
	}

	// Get the list of claim requests this config is being applied over.
	var requests []string
	for _, r := range results {
		requests = append(requests, r.Request)
	}

	// Get the list of allocatable devices this config is being applied over.
	allocatableDevices := make(AllocatableDevices)
	for _, r := range results {
		allocatableDevices[r.Device] = s.allocatable[r.Device]
	}

	if len(allocatableDevices) != 1 {
		return nil, fmt.Errorf("only expected 1 device for requests '%v' in claim '%v'", requests, claim.UID)
	}

	// Declare a device group state object to populate.
	configState := DeviceConfigState{
		Type:          ComputeDomainDaemonType,
		ComputeDomain: config.DomainID,
	}

	// Create new ComputeDomain daemon settings from the ComputeDomainManager.
	computeDomainDaemonSettings, err := s.computeDomainManager.NewSettings(config.DomainID)
	if err != nil {
		return nil, permanentError{fmt.Errorf("error creating ComputeDomain daemon settings: %w", err)}
	}

	// Prepare injecting IMEX daemon config files even if IMEX is not supported.
	// This for example creates
	// '/var/lib/kubelet/plugins/compute-domain.nvidia.com/domains/<uid>' on the
	// host which is used as mount source mapped to /imexd in the CD daemon
	// container.
	if err := computeDomainDaemonSettings.Prepare(ctx); err != nil {
		return nil, fmt.Errorf("error preparing ComputeDomain daemon settings for requests '%v' in claim '%v': %w", requests, claim.UID, err)
	}

	// Always inject CD config details into the CD daemon (regardless of clique
	// ID being empty or not).
	edits, err := computeDomainDaemonSettings.GetCDIContainerEditsCommon(ctx)
	if err != nil {
		return nil, fmt.Errorf("error getting common container edits for ComputeDomain daemon '%s': %w", config.DomainID, err)
	}
	configState.containerEdits = configState.containerEdits.Append(edits)

	// Only inject dev nodes related to
	// /proc/driver/nvidia/capabilities/fabric-imex-mgmt if IMEX is supported
	// (if we want to start the IMEX daemon process in the CD daemon pod).
	if s.computeDomainManager.CliqueID() != "" {
		nvcapPath := nvidiaCapFabricImexMgmtPath
		if common.UsingAltProcDevices() {
			nvcapPath = filepath.Join(s.config.flags.containerDriverRoot, "proc/driver/nvidia/capabilities/fabric-imex-mgmt")
		}

		// Parse the device node info for the fabric-imex-mgmt nvcap.
		nvcapDeviceInfo, err := common.ParseNVCapDeviceInfo(nvcapPath)
		if err != nil {
			return nil, fmt.Errorf("error parsing nvcap device info for fabric-imex-mgmt: %w", err)
		}
		edits := computeDomainDaemonSettings.GetCDIContainerEditsForImex(ctx, s.cdi.devRoot, nvcapDeviceInfo)
		configState.containerEdits = configState.containerEdits.Append(edits)
	}

	return &configState, nil
}

func (s *DeviceState) getConfigResultsMap(rcs *resourceapi.ResourceClaimStatus, decoder runtime.Decoder) (map[runtime.Object][]*resourceapi.DeviceRequestAllocationResult, error) {
	// Retrieve the full set of device configs for the driver.
	configs, err := GetOpaqueDeviceConfigs(
		decoder,
		DriverName,
		rcs.Allocation.Devices.Config,
	)
	if err != nil {
		return nil, fmt.Errorf("error getting opaque device configs: %w", err)
	}

	// Add the default ComputeDomainConfig to the front of the config list with the
	// lowest precedence. This guarantees there will be at least one of each
	// config in the list with len(Requests) == 0 for the lookup below.
	configs = slices.Insert(configs, 0, &OpaqueDeviceConfig{
		Requests: []string{},
		Config:   configapi.DefaultComputeDomainChannelConfig(),
	})
	configs = slices.Insert(configs, 0, &OpaqueDeviceConfig{
		Requests: []string{},
		Config:   configapi.DefaultComputeDomainDaemonConfig(),
	})

	// Look through the configs and figure out which one will be applied to
	// each device allocation result based on their order of precedence and type.
	configResultsMap := make(map[runtime.Object][]*resourceapi.DeviceRequestAllocationResult)
	for _, result := range rcs.Allocation.Devices.Results {
		if result.Driver != DriverName {
			continue
		}
		device, exists := s.allocatable[result.Device]
		if !exists {
			return nil, fmt.Errorf("requested device is not allocatable: %v", result.Device)
		}
		for _, c := range slices.Backward(configs) {
			if slices.Contains(c.Requests, result.Request) {
				if _, ok := c.Config.(*configapi.ComputeDomainChannelConfig); ok && device.Type() != ComputeDomainChannelType {
					return nil, fmt.Errorf("cannot apply ComputeDomainChannelConfig to request: %v", result.Request)
				}
				if _, ok := c.Config.(*configapi.ComputeDomainDaemonConfig); ok && device.Type() != ComputeDomainDaemonType {
					return nil, fmt.Errorf("cannot apply ComputeDomainDaemonConfig to request: %v", result.Request)
				}
				configResultsMap[c.Config] = append(configResultsMap[c.Config], &result)
				break
			}
			if len(c.Requests) == 0 {
				if _, ok := c.Config.(*configapi.ComputeDomainChannelConfig); ok && device.Type() != ComputeDomainChannelType {
					continue
				}
				if _, ok := c.Config.(*configapi.ComputeDomainDaemonConfig); ok && device.Type() != ComputeDomainDaemonType {
					continue
				}
				configResultsMap[c.Config] = append(configResultsMap[c.Config], &result)
				break
			}
		}
	}
	return configResultsMap, nil
}

// assertImexChannelNotAllocated() consults the absolute, node-local source of
// truth (the checkpoint data). It fails when the IMEX channel with ID `id` is
// already in use by another resource claim.
//
// Must be performed in the Prepare() path for any claim asking for a channel.
// This makes sure that Prepare() and Unprepare() calls acting on the same
// resource are processed in the correct order (this prevents for example
// unprepare-after-prepare, cf. issue 641).
//
// The implementation may become more involved when the same IMEX channel may be
// shared across pods on the same node).
func (s *DeviceState) assertImexChannelNotAllocated(id int) error {
	cp, err := s.getCheckpoint()
	if err != nil {
		return fmt.Errorf("unable to get checkpoint: %w", err)
	}

	for claimUID, claim := range cp.V2.PreparedClaims {
		// Ignore non-completed preparations: file-based locking guarantees that
		// only one Prepare() runs at any given time. If a claim is in the
		// `PrepareStarted` state then it is not actually currently in progress
		// of being prepared, but either retried soon (in which case we are
		// faster and win over it) or never retried (in which case we can also
		// safely allocate).
		if claim.CheckpointState != ClaimCheckpointStatePrepareCompleted {
			continue
		}

		for _, devs := range claim.PreparedDevices {
			for _, d := range devs.Devices {
				if d.Channel != nil && d.Channel.Info.ID == id {
					// Maybe log something based on `claim.Status.ReservedFor`
					// to facilitate debugging.
					return fmt.Errorf("channel %d already allocated by claim %s (according to checkpoint)", id, claimUID)
				}
			}
		}
	}
	return nil
}

// validateDriverVersionForIMEXDaemonsWithDNSNames validates that the driver version
// meets the minimum requirement for the IMEXDaemonsWithDNSNames feature gate.
func validateDriverVersionForIMEXDaemonsWithDNSNames(flags *Flags, nvdevlib *deviceLib) error {
	klog.Infof("Starting driver version validation for IMEXDaemonsWithDNSNames feature...")
	klog.Infof("Minimum required version: %s", IMEXDaemonsWithDNSNamesMinDriverVersion)

	driverVer, err := nvdevlib.getDriverVersion()
	if err != nil {
		return fmt.Errorf("error getting driver version: %w", err)
	}

	minVersion, err := version.ParseGeneric(IMEXDaemonsWithDNSNamesMinDriverVersion)
	if err != nil {
		return fmt.Errorf("error parsing minimum version: %w", err)
	}

	if driverVer.LessThan(minVersion) {
		klog.Errorf("IMEXDaemonsWithDNSNames feature requires GPU driver version >= %s, but found %s", minVersion.String(), driverVer.String())
		klog.Errorf("If installed via helm, set featureGates.IMEXDaemonsWithDNSNames=false to disable")
		return fmt.Errorf("minimum version not satisfied for IMEXDaemonsWithDNSNames feature")
	}

	klog.Infof("Driver version validation passed: %s >= %s", driverVer.String(), minVersion.String())
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
			// Bad opaque config: i) do not retry preparing this resource
			// internally and ii) return notion of permanent error to kubelet,
			// to give it an opportunity to play this error back to the user so
			// that it becomes actionable.
			return nil, permanentError{fmt.Errorf("error decoding config parameters: %w", err)}
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
				return fmt.Errorf(
					"requested device %s is already allocated to different claim %s",
					device, existingClaimUID,
				)
			}
		}
	}
	return nil
}

func syncPreparedDevicesGaugeFromCheckpoint(nodeName string, cp *Checkpoint) {
	counts := make(map[string]int)
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

	for _, dt := range []string{ComputeDomainChannelType, ComputeDomainDaemonType, UnknownDeviceType} {
		if count, ok := counts[dt]; !ok {
			drametrics.SetPreparedDevicesCounts(nodeName, DriverName, dt, 0)
		} else {
			drametrics.SetPreparedDevicesCounts(nodeName, DriverName, dt, count)
		}
	}
}
