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
	"encoding/json"
	"fmt"
	"time"

	resourceapi "k8s.io/api/resource/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/util/retry"
	"k8s.io/dynamic-resource-allocation/kubeletplugin"
	"k8s.io/klog/v2"

	"sigs.k8s.io/dra-driver-nvidia-gpu/pkg/featuregates"
)

// The status write is best-effort and must never wedge the prepare/unprepare
// path (it runs while the DeviceState lock and the node-global prep/unprep
// flock are held): bound each publish attempt, mirroring the 20s bound on the
// claim GET in cleanup.go.
const deviceStatusUpdateTimeout = 10 * time.Second

// DeviceStatusData is the driver-specific payload published as
// ResourceClaim.status.devices[].data (KEP-4817). It records which concrete
// device backed an allocation: unlike ResourceSlice contents, it remains
// available after the device disappears from the slice (e.g. when a MIG
// device is torn down or a GPU becomes unhealthy), so past allocations can
// still be correlated with later health or scheduling issues.
type DeviceStatusData struct {
	Type          string `json:"type"`
	UUID          string `json:"uuid,omitempty"`
	ProductName   string `json:"productName,omitempty"`
	DriverVersion string `json:"driverVersion,omitempty"`
	// For MIG devices, the PCI bus ID is that of the parent GPU.
	PCIBusID string `json:"pciBusID,omitempty"`
	// MIG devices only.
	MigProfile string `json:"migProfile,omitempty"`
	ParentUUID string `json:"parentUUID,omitempty"`
}

// publishDeviceStatuses writes one AllocatedDeviceStatus per device prepared
// for the claim into ResourceClaim.status.devices. Failure to publish is
// non-fatal for Prepare(): the devices are prepared either way and the claim
// status is simply missing the data; the write is retried on the next
// (idempotent) Prepare() call for the claim. A successful publish is recorded
// in memory so that repeated Prepare() calls for the claim (one per pod
// referencing it, plus the replay after a kubelet or plugin restart) skip the
// API round-trip.
//
// Must be called with the DeviceState lock held (it serializes access to
// statusPublishedClaims and the allocatable device map).
func (s *DeviceState) publishDeviceStatuses(ctx context.Context, claim *resourceapi.ResourceClaim, preparedDevices PreparedDevices) {
	if !featuregates.Enabled(featuregates.ResourceClaimDeviceStatus) {
		return
	}
	claimUID := string(claim.UID)
	if s.statusPublishedClaims[claimUID] {
		return
	}

	statuses := s.buildDeviceStatuses(claim, preparedDevices)
	if len(statuses) == 0 {
		return
	}

	written, err := s.updateDeviceStatuses(ctx, claim, statuses)
	if err != nil {
		klog.Errorf("Failed to publish device statuses to claim %s: %s", ResourceClaimToString(claim), err)
		return
	}
	if s.statusPublishedClaims == nil {
		s.statusPublishedClaims = make(map[string]bool)
	}
	s.statusPublishedClaims[claimUID] = true
	if written {
		klog.V(4).Infof("Published %d device status(es) to claim %s", len(statuses), ResourceClaimToString(claim))
	} else {
		klog.V(4).Infof("Device status(es) for claim %s already up to date", ResourceClaimToString(claim))
	}
}

// clearDeviceStatuses removes this driver's entries from
// ResourceClaim.status.devices. Called during Unprepare so that a claim that
// outlives its preparation (e.g. a user-created claim, or one shared by
// several pods) does not keep advertising devices that are no longer
// prepared. Best-effort: an error only means the stale entries linger until
// the claim is deallocated, at which point the API server drops them.
//
// Must be called with the DeviceState lock held.
func (s *DeviceState) clearDeviceStatuses(ctx context.Context, claimRef kubeletplugin.NamespacedObject) {
	if !featuregates.Enabled(featuregates.ResourceClaimDeviceStatus) {
		return
	}
	delete(s.statusPublishedClaims, string(claimRef.UID))

	ctx, cancel := context.WithTimeout(ctx, deviceStatusUpdateTimeout)
	defer cancel()

	rc := s.config.clientsets.Resource.ResourceClaims(claimRef.Namespace)
	cleared := 0
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		current, err := rc.Get(ctx, claimRef.Name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if current.UID != claimRef.UID {
			// The claim was deleted and recreated: nothing of ours to clear.
			return nil
		}
		merged := mergeDeviceStatuses(current.Status.Devices, nil)
		cleared = len(current.Status.Devices) - len(merged)
		if cleared == 0 {
			return nil
		}
		updated := current.DeepCopy()
		updated.Status.Devices = merged
		_, err = rc.UpdateStatus(ctx, updated, metav1.UpdateOptions{})
		return err
	})
	switch {
	case apierrors.IsNotFound(err):
		klog.V(4).Infof("Clear device statuses: claim %s already gone", claimRef.String())
	case err != nil:
		klog.Errorf("Failed to clear device statuses on claim %s: %s", claimRef.String(), err)
	case cleared > 0:
		klog.V(4).Infof("Cleared %d device status(es) from claim %s", cleared, claimRef.String())
	default:
		klog.V(4).Infof("Clear device statuses: no entries of ours on claim %s", claimRef.String())
	}
}

// buildDeviceStatuses constructs one AllocatedDeviceStatus per allocation
// result owned by this driver, with a DeviceStatusData payload describing the
// concrete prepared device. Device properties are resolved from the current
// device discovery data (not from the checkpointed PreparedDevices, which
// only round-trip a subset of fields through JSON).
func (s *DeviceState) buildDeviceStatuses(claim *resourceapi.ResourceClaim, preparedDevices PreparedDevices) []resourceapi.AllocatedDeviceStatus {
	if claim.Status.Allocation == nil {
		return nil
	}

	preparedByName := make(map[DeviceName]*PreparedDevice)
	for _, group := range preparedDevices {
		for i := range group.Devices {
			device := &group.Devices[i]
			if name, ok := preparedDeviceName(device); ok {
				preparedByName[name] = device
			}
		}
	}

	var statuses []resourceapi.AllocatedDeviceStatus
	// status.devices is validated by the API server as a set keyed by
	// (driver, pool, device, shareID): a duplicate key (e.g. two allocation
	// results for the same device without a ShareID, as produced by
	// adminAccess requests) would make the whole update be rejected.
	seen := make(map[string]bool)
	for _, result := range claim.Status.Allocation.Devices.Results {
		if result.Driver != DriverName {
			continue
		}
		device, exists := preparedByName[result.Device]
		if !exists {
			continue
		}
		key := result.Pool + "/" + result.Device
		if result.ShareID != nil {
			key += "/" + string(*result.ShareID)
		}
		if seen[key] {
			continue
		}
		seen[key] = true

		data, err := json.Marshal(s.buildDeviceStatusData(device))
		if err != nil {
			// Not expected (plain struct); skip the entry rather than
			// publishing an empty payload.
			klog.Errorf("Failed to marshal device status data for device %s: %s", result.Device, err)
			continue
		}

		statuses = append(statuses, resourceapi.AllocatedDeviceStatus{
			Driver: result.Driver,
			Pool:   result.Pool,
			Device: result.Device,
			// With ConsumableShares, the same device may appear in more than
			// one result; the ShareID distinguishes the entries.
			ShareID: (*string)(result.ShareID),
			Data:    &runtime.RawExtension{Raw: data},
		})
	}
	return statuses
}

// preparedDeviceName returns the announced device name for a prepared device.
// Unlike PreparedDevice.CanonicalName() it does not panic on malformed
// (e.g. checkpoint-restored) entries.
func preparedDeviceName(device *PreparedDevice) (DeviceName, bool) {
	switch device.Type() {
	case GpuDeviceType:
		if device.Gpu.Device != nil {
			return device.Gpu.Device.DeviceName, true
		}
	case PreparedMigDeviceType:
		if device.Mig.Device != nil {
			return device.Mig.Device.DeviceName, true
		}
	case VfioDeviceType:
		if device.Vfio.Device != nil {
			return device.Vfio.Device.DeviceName, true
		}
	}
	return "", false
}

func (s *DeviceState) buildDeviceStatusData(device *PreparedDevice) DeviceStatusData {
	switch device.Type() {
	case GpuDeviceType:
		data := DeviceStatusData{
			Type: GpuDeviceType,
			UUID: device.Gpu.Info.UUID,
		}
		if gpu := s.nvdevlib.gpuInfosByUUID[device.Gpu.Info.UUID]; gpu != nil {
			data.ProductName = gpu.productName
			data.DriverVersion = gpu.driverVersion
			data.PCIBusID = gpu.pciBusID
		}
		return data
	case PreparedMigDeviceType:
		data := DeviceStatusData{
			Type: PreparedMigDeviceType,
		}
		if device.Mig.Concrete != nil {
			data.UUID = device.Mig.Concrete.MigUUID
			data.ParentUUID = device.Mig.Concrete.ParentUUID
			if parent := s.nvdevlib.gpuInfosByUUID[device.Mig.Concrete.ParentUUID]; parent != nil {
				data.ProductName = parent.productName
				data.DriverVersion = parent.driverVersion
				data.PCIBusID = parent.pciBusID
			}
		}
		if allocatable := s.perGPUAllocatable.GetAllocatableDevice(device.Mig.Device.DeviceName); allocatable != nil {
			switch {
			case allocatable.MigStatic != nil:
				data.MigProfile = allocatable.MigStatic.Profile
			case allocatable.MigDynamic != nil:
				data.MigProfile = allocatable.MigDynamic.Profile.String()
			}
		}
		return data
	case VfioDeviceType:
		data := DeviceStatusData{
			Type:     VfioDeviceType,
			UUID:     device.Vfio.Info.UUID,
			PCIBusID: device.Vfio.Info.PciBusID,
		}
		if allocatable := s.perGPUAllocatable.GetAllocatableDevice(device.Vfio.Device.DeviceName); allocatable != nil && allocatable.Vfio != nil {
			data.ProductName = allocatable.Vfio.productName
			if data.PCIBusID == "" {
				data.PCIBusID = allocatable.Vfio.PciBusID
			}
		}
		return data
	}
	return DeviceStatusData{Type: UnknownDeviceType}
}

// updateDeviceStatuses updates ResourceClaim.status.devices with the given
// entries, replacing only entries owned by this driver. Entries written by
// other drivers (a claim can carry allocations from several drivers) are
// preserved -- deliberately stricter than dra-example-driver, which
// overwrites status.devices wholesale. Returns whether an update was
// written (false when the claim already carried identical entries).
func (s *DeviceState) updateDeviceStatuses(ctx context.Context, claim *resourceapi.ResourceClaim, statuses []resourceapi.AllocatedDeviceStatus) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, deviceStatusUpdateTimeout)
	defer cancel()

	rc := s.config.clientsets.Resource.ResourceClaims(claim.Namespace)

	written := false
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		current, err := rc.Get(ctx, claim.Name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if current.UID != claim.UID {
			return fmt.Errorf("claim UID changed (prepared %s, found %s): not writing device status", claim.UID, current.UID)
		}

		merged := mergeDeviceStatuses(current.Status.Devices, statuses)
		if apiequality.Semantic.DeepEqual(current.Status.Devices, merged) {
			// Nothing to do; common on idempotent Prepare() retries.
			return nil
		}

		updated := current.DeepCopy()
		updated.Status.Devices = merged
		result, err := rc.UpdateStatus(ctx, updated, metav1.UpdateOptions{})
		if err != nil {
			return err
		}
		written = true
		if len(result.Status.Devices) == 0 {
			// The API server accepted the write but dropped the field: the
			// cluster-side DRAResourceClaimDeviceStatus feature gate is
			// disabled. Warn (the driver-side gate was enabled explicitly)
			// but report success so the write is not retried forever.
			klog.Warningf("Device statuses for claim %s were dropped by the API server; "+
				"the DRAResourceClaimDeviceStatus Kubernetes feature gate appears to be disabled on this cluster",
				ResourceClaimToString(claim))
		}
		return nil
	})
	return written, err
}

// mergeDeviceStatuses returns the entries of `existing` not owned by this
// driver, followed by `ours` (the full replacement set for this driver's
// entries; nil removes them all).
func mergeDeviceStatuses(existing, ours []resourceapi.AllocatedDeviceStatus) []resourceapi.AllocatedDeviceStatus {
	var merged []resourceapi.AllocatedDeviceStatus
	for _, status := range existing {
		if status.Driver != DriverName {
			merged = append(merged, status)
		}
	}
	return append(merged, ours...)
}
