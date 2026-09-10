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
	"encoding/binary"
	"fmt"
	"strconv"
	"strings"
	"sync"

	"github.com/NVIDIA/go-nvml/pkg/nvml"
	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/klog/v2"

	"sigs.k8s.io/dra-driver-nvidia-gpu/pkg/featuregates"
)

const (
	FullGPUInstanceID uint32 = 0xFFFFFFFF
)

const (
	TaintKeyXID         = DriverName + "/xid"
	TaintKeyGPULost     = DriverName + "/gpu-lost"
	TaintKeyUnmonitored = DriverName + "/unmonitored"
)

// DeviceHealthEventType classifies the category of health event detected by
// the NVML health monitor.
type DeviceHealthEventType string

const (
	HealthEventXID         DeviceHealthEventType = "xid"
	HealthEventGPULost     DeviceHealthEventType = "gpu-lost"
	HealthEventUnmonitored DeviceHealthEventType = "unmonitored"
)

// DeviceHealthEvent carries a typed health notification from the NVML health
// monitor to the driver's event handler, enabling the driver to set the
// appropriate DRA device taint per the Option A schema (KEP-5055).
// Devices is a batch: for GPU_LOST and unmonitored events where all affected devices
// are aggregated into a single event so the consumer applies one ResourceSlice
// update instead of N.
type DeviceHealthEvent struct {
	Devices   []*AllocatableDevice
	EventType DeviceHealthEventType
	// inspired by NVML Event type and only meaningful for xid errors.
	// may have to create a custom type based on future device-api
	EventData uint64
}

// healthEventToTaint maps a DeviceHealthEvent to the corresponding DRA
// DeviceTaint using the Option A taint key schema: one key per health
// dimension under the gpu.nvidia.com domain.
func healthEventToTaint(monitor deviceHealthMonitor, event *DeviceHealthEvent) *resourceapi.DeviceTaint {
	switch event.EventType {
	case HealthEventXID:
		effect := resourceapi.DeviceTaintEffectNoSchedule
		if monitor != nil && monitor.IsEventNonFatal(event) {
			effect = resourceapi.DeviceTaintEffectNone
		}
		return &resourceapi.DeviceTaint{
			Key:    TaintKeyXID,
			Value:  strconv.FormatUint(event.EventData, 10),
			Effect: effect,
		}
	case HealthEventGPULost:
		return &resourceapi.DeviceTaint{
			Key:    TaintKeyGPULost,
			Effect: resourceapi.DeviceTaintEffectNoSchedule,
		}
	case HealthEventUnmonitored:
		return &resourceapi.DeviceTaint{
			Key:    TaintKeyUnmonitored,
			Effect: resourceapi.DeviceTaintEffectNone,
		}
	default:
		klog.Errorf("Unknown health event type %q, defaulting to unmonitored taint", event.EventType)
		return &resourceapi.DeviceTaint{
			Key:    TaintKeyUnmonitored,
			Effect: resourceapi.DeviceTaintEffectNone,
		}
	}
}

type nvmlDeviceHealthMonitor struct {
	nvmllib           nvml.Interface
	eventSet          nvml.EventSet
	unhealthy         chan *DeviceHealthEvent
	perGPUAllocatable *PerGPUAllocatableDevices
	gpuInfosByUUID    map[string]*GpuInfo
	skippedXids       map[uint64]bool
	wg                sync.WaitGroup
}

func newNvmlDeviceHealthMonitor(config *Config, perGPUAllocatable *PerGPUAllocatableDevices, nvdevlib *deviceLib) (*nvmlDeviceHealthMonitor, error) {
	if nvdevlib.nvmllib == nil {
		return nil, fmt.Errorf("nvml library is nil")
	}
	if ret := nvdevlib.nvmllib.Init(); ret != nvml.SUCCESS {
		return nil, fmt.Errorf("failed to initialize NVML: %w", ret)
	}
	defer func() {
		_ = nvdevlib.nvmllib.Shutdown()
	}()

	if perGPUAllocatable == nil {
		return nil, fmt.Errorf("perGPUAllocatable is nil")
	}
	all := perGPUAllocatable.GetAllDevices()
	m := &nvmlDeviceHealthMonitor{
		nvmllib:           nvdevlib.nvmllib,
		unhealthy:         make(chan *DeviceHealthEvent, len(all)),
		perGPUAllocatable: perGPUAllocatable,
		gpuInfosByUUID:    nvdevlib.gpuInfosByUUID,
		skippedXids:       xidsToSkip(config.flags.additionalXidsToIgnore),
	}
	return m, nil
}

// RegisterEvents creates the NVML event set and starts recording events for
// every physical parent GPU before the kubelet server accepts requests.
func (m *nvmlDeviceHealthMonitor) RegisterEvents() (rerr error) {
	if ret := m.nvmllib.Init(); ret != nvml.SUCCESS {
		return fmt.Errorf("failed to initialize NVML: %w", ret)
	}

	defer func() {
		if rerr != nil {
			_ = m.nvmllib.Shutdown()
		}
	}()

	klog.V(4).Info("creating NVML events for device health monitor")
	eventSet, ret := m.nvmllib.EventSetCreate()
	if ret != nvml.SUCCESS {
		return fmt.Errorf("failed to create event set: %w", ret)
	}

	m.eventSet = eventSet

	klog.V(4).Info("registering NVML events for device health monitor")
	m.registerEventsForDevices()
	return nil
}

// Start launches the NVML event wait loop after RegisterEvents has completed.
func (m *nvmlDeviceHealthMonitor) Start(ctx context.Context) error {
	if m.eventSet == nil {
		return fmt.Errorf("NVML events have not been registered")
	}
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		m.run(ctx)
	}()

	klog.V(4).Info("started device health monitoring")
	return nil
}

func (m *nvmlDeviceHealthMonitor) registerEventsForDevices() {
	eventMask := uint64(nvml.EventTypeXidCriticalError | nvml.EventTypeDoubleBitEccError | nvml.EventTypeSingleBitEccError)

	for pciBusID, devices := range m.perGPUAllocatable.allocatablesMap {
		gpu, ret := m.nvmllib.DeviceGetHandleByPciBusId(string(pciBusID))
		if ret != nvml.SUCCESS {
			klog.Warningf("Unable to get device handle from PCI Bus ID[%s]: %v; marking devices as unmonitored", pciBusID, ret)
			m.sendHealthEventForDevices(devices, HealthEventUnmonitored)
			continue
		}

		supportedEvents, ret := gpu.GetSupportedEventTypes()
		if ret != nvml.SUCCESS {
			klog.Warningf("unable to determine the supported events for %s: %v; marking devices as unmonitored", pciBusID, ret)
			m.sendHealthEventForDevices(devices, HealthEventUnmonitored)
			continue
		}

		ret = gpu.RegisterEvents(eventMask&supportedEvents, m.eventSet)
		if ret == nvml.ERROR_NOT_SUPPORTED {
			klog.Warningf("Device %v is too old to support healthchecking.", pciBusID)
			m.sendHealthEventForDevices(devices, HealthEventUnmonitored)
		} else if ret != nvml.SUCCESS {
			klog.Warningf("unable to register events for %s: %v; marking devices as unmonitored", pciBusID, ret)
			m.sendHealthEventForDevices(devices, HealthEventUnmonitored)
		}
	}
}

func (m *nvmlDeviceHealthMonitor) Stop() {
	if m == nil {
		return
	}
	klog.V(6).Info("stopping health monitor")

	m.wg.Wait()

	if ret := m.eventSet.Free(); ret != nvml.SUCCESS {
		klog.Warningf("failed to unset events: %v", ret)
	}

	if ret := m.nvmllib.Shutdown(); ret != nvml.SUCCESS {
		klog.Warningf("failed to shutdown NVML: %v", ret)
	}
	close(m.unhealthy)
}

func (m *nvmlDeviceHealthMonitor) run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			klog.V(6).Info("Stopping event-driven GPU health monitor...")
			return
		default:
			event, ret := m.eventSet.Wait(5000) // timeout in 5000 ms.
			if ret == nvml.ERROR_TIMEOUT {
				continue
			}
			// not all return errors are handled as currently there is no proper way to process these errors other than marking all devices healthy.
			// Ref doc: [https://docs.nvidia.com/deploy/nvml-api/group__nvmlEvents.html#group__nvmlEvents_1g9714b0ca9a34c7a7780f87fee16b205c].
			if ret != nvml.SUCCESS {
				if ret == nvml.ERROR_GPU_IS_LOST {
					klog.Warningf("GPU is lost error: %v; Tainting all devices with %s", ret, TaintKeyGPULost)
					m.sendHealthEventForAllDevices(HealthEventGPULost)
					continue
				}
				klog.V(6).Infof("Error waiting for NVML event: %v. Retrying...", ret)
				continue
			}

			// TODO: check why other supported types are not considered?
			eType := event.EventType
			xid := event.EventData
			gi := event.GpuInstanceId
			ci := event.ComputeInstanceId
			if eType != nvml.EventTypeXidCriticalError {
				klog.V(6).Infof("Skipping non-nvmlEventTypeXidCriticalError event: Data=%d, Type=%d, GI=%d, CI=%d", xid, eType, gi, ci)
				continue
			}

			klog.V(4).Infof("Processing event XID=%d event", xid)
			// this seems an extreme action.
			// should we just log the error and proceed anyway.
			// TODO: look into how to properly handle this error.
			eventUUID, ret := event.Device.GetUUID()
			if ret != nvml.SUCCESS {
				klog.Warningf("Failed to determine uuid for event %v: %v; Tainting all devices with %s", event, ret, TaintKeyGPULost)
				m.sendHealthEventForAllDevices(HealthEventGPULost)
				continue
			}
			affectedDevice, err := m.resolveDeviceByEventAddress(eventUUID, event.Device, gi, ci)
			// An error indicates inconsistent UUID/PCI inventory. A nil device
			// without an error means the event's GI/CI is not available.
			if err != nil {
				klog.Warningf("Unable to resolve XID=%d event for UUID:%s, GI:%d, CI:%d: %v", xid, eventUUID, gi, ci, err)
				continue
			}
			if affectedDevice == nil {
				klog.V(6).Infof("Ignoring event for unexpected device (UUID:%s, GI:%d, CI:%d)", eventUUID, gi, ci)
				continue
			}

			klog.V(4).Infof("Sending XID=%d health event for device %s", xid, affectedDevice.CanonicalName())
			m.unhealthy <- &DeviceHealthEvent{
				Devices:   []*AllocatableDevice{affectedDevice},
				EventType: HealthEventXID,
				EventData: xid,
			}
		}
	}
}

func (m *nvmlDeviceHealthMonitor) Unhealthy() <-chan *DeviceHealthEvent {
	return m.unhealthy
}

// sendHealthEventForAllDevices aggregates every device across all GPUs into a
// single batched DeviceHealthEvent so the consumer makes one ResourceSlice
// update.
func (m *nvmlDeviceHealthMonitor) sendHealthEventForAllDevices(eventType DeviceHealthEventType) {
	m.sendBatchedHealthEvent(m.perGPUAllocatable.GetAllDevices().List(), eventType)
}

// sendHealthEventForDevices aggregates all devices under a single parent GPU
// into one batched DeviceHealthEvent.
func (m *nvmlDeviceHealthMonitor) sendHealthEventForDevices(devices AllocatableDevices, eventType DeviceHealthEventType) {
	m.sendBatchedHealthEvent(devices.List(), eventType)
}

// NVML identifies a MIG-scoped event by the tuple (parent UUID, GPU instance
// ID, compute instance ID). A full-GPU event reports FullGPUInstanceID for both
// the GPU instance ID and compute instance ID.
//
// resolveDeviceByEventAddress maps this address, extracted from an NVML event,
// to an advertised allocatable device.
func (m *nvmlDeviceHealthMonitor) resolveDeviceByEventAddress(parentUUID string, eventDevice nvml.Device, gi, ci uint32) (*AllocatableDevice, error) {
	parent, ok := m.gpuInfosByUUID[parentUUID]
	if !ok {
		return nil, fmt.Errorf("failed to find parent GPU UUID %s in the discovered GPU inventory", parentUUID)
	}

	devices, ok := m.perGPUAllocatable.allocatablesMap[parent.pciBusID]
	if !ok {
		return nil, fmt.Errorf("failed to find PCI Bus ID %s for parent GPU UUID %s in the allocatable inventory", parent.pciBusID, parent.UUID)
	}

	switch {
	case gi == FullGPUInstanceID && ci == FullGPUInstanceID:
		return devices.GetGPUDeviceByUUID(parentUUID), nil

	case gi != FullGPUInstanceID && ci != FullGPUInstanceID:
		if featuregates.Enabled(featuregates.DynamicMIG) {
			spec, err := resolveMigEvent(eventDevice, parent, gi, ci)
			if err != nil {
				return nil, fmt.Errorf("failed to resolve Dynamic MIG device for parent %s, GI=%d, CI=%d: %w", parentUUID, gi, ci, err)
			}
			return devices.GetMigDynamicDeviceByTuple(spec), nil
		}

		return devices.GetMigStaticDeviceByLiveTuple(&MigLiveTuple{
			ParentUUID: parentUUID,
			GIID:       int(gi),
			CIID:       int(ci),
		}), nil

	default:
		// A GI can exist without a CI, but it does not represent a usable,
		// allocatable MIG device. Similar to device plugin, treat an event
		// as MIG-scoped only when both GI and CI are present.
		//
		// See:
		// https://github.com/NVIDIA/k8s-device-plugin/blob/main/internal/rm/health.go#L160
		// https://docs.nvidia.com/deploy/nvml-api/structnvmlEventData__t.html
		// https://docs.nvidia.com/datacenter/tesla/mig-user-guide/latest/getting-started-with-mig.html#creating-gpu-instances
		klog.V(6).Infof("Ignoring NVML event with inconsistent instance address for parent UUID %s: GI=%d, CI=%d", parentUUID, gi, ci)
		return nil, nil
	}
}

// resolveMigEvent translates the live GI/CI address reported by NVML into the
// abstract profile and placement used to advertise a Dynamic MIG device.
func resolveMigEvent(device nvml.Device, parent *GpuInfo, giID, ciID uint32) (*MigSpecTuple, error) {
	gi, ret := device.GetGpuInstanceById(int(giID))
	if ret != nvml.SUCCESS {
		return nil, fmt.Errorf("failed to get GPU instance %d: %w", giID, ret)
	}
	giInfo, ret := gi.GetInfo()
	if ret != nvml.SUCCESS {
		return nil, fmt.Errorf("failed to get info for GPU instance %d: %w", giID, ret)
	}

	_, ret = gi.GetComputeInstanceById(int(ciID))
	if ret != nvml.SUCCESS {
		return nil, fmt.Errorf("failed to get compute instance %d in GPU instance %d: %w", ciID, giID, ret)
	}

	klog.V(6).Infof(
		"Resolved Dynamic MIG event (UUID:%s, GI:%d, CI:%d) to profile ID %d, placement start %d",
		parent.UUID, giID, ciID, giInfo.ProfileId, giInfo.Placement.Start,
	)
	return &MigSpecTuple{
		ParentMinor:    parent.minor,
		ParentPCIBusID: parent.pciBusID,
		ProfileID:      int(giInfo.ProfileId),
		PlacementStart: int(giInfo.Placement.Start),
	}, nil
}

// sendBatchedHealthEvent sends a single DeviceHealthEvent containing all
// affected devices. Uses a non-blocking send to protect the monitor goroutine
// from deadlocks when the channel is full.
func (m *nvmlDeviceHealthMonitor) sendBatchedHealthEvent(devices []*AllocatableDevice, eventType DeviceHealthEventType) {
	if len(devices) == 0 {
		return
	}
	event := &DeviceHealthEvent{
		Devices:   devices,
		EventType: eventType,
	}
	select {
	case m.unhealthy <- event:
		klog.V(6).Infof("Sent batched %s health event for %d device(s)", eventType, len(devices))
	default:
		klog.Errorf("Health event channel full; dropping batched %s event for %d device(s)", eventType, len(devices))
	}
}

// xidsToSkip returns the XIDs explicitly configured by the administrator.
//
// Earlier versions also treated the following XIDs as non-fatal by default.
// The NVIDIA XID Catalog documents their immediate actions as:
//
//   - XID 13, Graphics Engine Exception: RESTART_APP.
//   - XID 31, GPU memory page fault: RESTART_APP.
//   - XID 43, GPU stopped processing: IGNORE.
//   - XID 45, Preemptive cleanup due to previous errors: WORKFLOW_XID_45.
//   - XID 68, NVDEC0 Exception: RESTART_APP.
//   - XID 109, Context Switch Timeout Error: RESET_GPU.
//
// The built-in list is no longer used for classification. The parent GPU's
// current recovery action determines the scheduling impact. This configured
// list remains as an explicit administrator override.
//
// See:
// https://docs.nvidia.com/deploy/xid-errors/latest/analyzing-xid-catalog.html
func xidsToSkip(input string) map[uint64]bool {
	skippedXids := make(map[uint64]bool)
	if input == "" {
		return skippedXids
	}

	klog.V(6).Infof("Creating a list of XIDs to ignore: [%s]", input)

	for _, value := range strings.Split(input, ",") {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}

		xid, err := strconv.ParseUint(value, 10, 64)
		if err != nil {
			klog.V(6).Infof("Ignoring malformed XID value %q: %v", value, err)
			continue
		}

		skippedXids[xid] = true
	}

	return skippedXids
}

// queryGPURecoveryAction reads the current recovery action reported by NVML.
func queryGPURecoveryAction(device nvml.Device) (nvml.DeviceGpuRecoveryAction, error) {
	values := []nvml.FieldValue{{
		FieldId: nvml.FI_DEV_GET_GPU_RECOVERY_ACTION,
	}}

	if ret := device.GetFieldValues(values); ret != nvml.SUCCESS {
		return nvml.GPU_RECOVERY_ACTION_NONE, fmt.Errorf("failed to query GPU recovery action: %w", ret)
	}

	value := values[0]
	if ret := nvml.Return(value.NvmlReturn); ret != nvml.SUCCESS {
		return nvml.GPU_RECOVERY_ACTION_NONE, fmt.Errorf("failed to read GPU recovery action field: %w", ret)
	}
	if nvml.ValueType(value.ValueType) != nvml.VALUE_TYPE_UNSIGNED_INT {
		return nvml.GPU_RECOVERY_ACTION_NONE, fmt.Errorf("failed to decode GPU recovery action: unexpected value type %d", value.ValueType)
	}

	// NVML stores this unsigned-int field in the first four bytes of its 8-byte
	// value union using host byte order.
	return nvml.DeviceGpuRecoveryAction(binary.NativeEndian.Uint32(value.Value[:4])), nil
}

func gpuRecoveryActionString(action nvml.DeviceGpuRecoveryAction) string {
	switch action {
	case nvml.GPU_RECOVERY_ACTION_NONE:
		return "None"
	case nvml.GPU_RECOVERY_ACTION_GPU_RESET:
		return "GPU Reset Required"
	case nvml.GPU_RECOVERY_ACTION_NODE_REBOOT:
		return "Node Reboot Required"
	case nvml.GPU_RECOVERY_ACTION_DRAIN_P2P:
		return "Drain P2P"
	case nvml.GPU_RECOVERY_ACTION_DRAIN_AND_RESET:
		return "Drain and Reset"
	case nvml.GPU_RECOVERY_ACTION_RECOVER_IMEX_DOMAIN:
		return "Recover IMEX Domain"
	default:
		return fmt.Sprintf("Unknown (%d)", action)
	}
}

// IsEventNonFatal classifies an XID using the parent GPU's current recovery
// action. An administrator-configured XID remains non-fatal, but the recovery
// action is still queried and logged.
func (m *nvmlDeviceHealthMonitor) IsEventNonFatal(event *DeviceHealthEvent) bool {
	if event.EventType != HealthEventXID {
		return false
	}

	xid := event.EventData
	ignored := m.skippedXids[xid]

	pciBusID := event.Devices[0].GetGPUPCIBusID()
	device, ret := m.nvmllib.DeviceGetHandleByPciBusId(pciBusID)
	if ret != nvml.SUCCESS {
		klog.Warningf("Failed to get parent GPU handle for PCI bus ID %s while processing XID=%d: %v", pciBusID, xid, ret)
		return ignored
	}

	action, err := queryGPURecoveryAction(device)
	if err != nil {
		klog.Warningf("Failed to query GPU recovery action while processing XID=%d: %v", xid, err)
		return ignored
	}

	if ignored {
		klog.Warningf("XID %d: NVML reported GPU recovery action=%q; treating the event as non-fatal because the XID is configured via --additional-xids-to-ignore", xid, gpuRecoveryActionString(action))
		return true
	}

	switch action {
	case nvml.GPU_RECOVERY_ACTION_NONE:
		klog.V(4).Infof("XID %d: NVML reported GPU recovery action=%q; treating the event as non-fatal", xid, gpuRecoveryActionString(action))
		return true

	case nvml.GPU_RECOVERY_ACTION_RECOVER_IMEX_DOMAIN:
		// RECOVER_IMEX_DOMAIN requests recovery of the IMEX domain rather than
		// recovery of the local GPU device. Keep the XID informational for GPU
		// scheduling and surface the recovery requirement in the log.
		klog.Warningf("XID %d: NVML reports GPU recovery action=%q; the action is scoped to IMEX-domain recovery, so treating the event as non-fatal for GPU scheduling", xid, gpuRecoveryActionString(action))
		return true

	default:
		klog.Warningf("XID %d: NVML reported GPU recovery action=%q; treating the event as fatal", xid, gpuRecoveryActionString(action))
		return false
	}
}
