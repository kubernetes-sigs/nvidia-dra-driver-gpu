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
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

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

// logIfUnknownType reports an event type the driver does not know, once per
// received event; such events are treated as unmonitored by both consumers.
func (e *DeviceHealthEvent) logIfUnknownType() {
	switch e.EventType {
	case HealthEventXID, HealthEventGPULost, HealthEventUnmonitored:
	default:
		klog.Errorf("Unknown health event type %q, treating as unmonitored", e.EventType)
	}
}

// IsCritical reports whether the event is a hardware failure that makes the
// device unusable until fixed: a lost GPU, or an XID that the monitor does
// not classify as non-fatal. It is the single decision behind both the
// device taint derived from the event (healthEventToTaint, KEP-5055) and the
// device health reported to the kubelet (deviceHealthFromTaints, KEP-4680), so
// the two cannot drift apart.
func (e *DeviceHealthEvent) IsCritical(monitor deviceHealthMonitor) bool {
	switch e.EventType {
	case HealthEventXID:
		return monitor == nil || !monitor.IsEventNonFatal(e)
	case HealthEventGPULost:
		return true
	default:
		return false
	}
}

// healthEventToTaint maps a DeviceHealthEvent to the corresponding DRA
// DeviceTaint using the Option A taint key schema: one key per health
// dimension under the gpu.nvidia.com domain.
func healthEventToTaint(monitor deviceHealthMonitor, event *DeviceHealthEvent) *resourceapi.DeviceTaint {
	effect := resourceapi.DeviceTaintEffectNone
	if event.IsCritical(monitor) {
		effect = resourceapi.DeviceTaintEffectNoSchedule
	}
	switch event.EventType {
	case HealthEventXID:
		return &resourceapi.DeviceTaint{
			Key:    TaintKeyXID,
			Value:  strconv.FormatUint(event.EventData, 10),
			Effect: effect,
		}
	case HealthEventGPULost:
		return &resourceapi.DeviceTaint{
			Key:    TaintKeyGPULost,
			Effect: effect,
		}
	default:
		// HealthEventUnmonitored and unknown event types.
		return &resourceapi.DeviceTaint{
			Key:    TaintKeyUnmonitored,
			Effect: effect,
		}
	}
}

// monitorHeartbeatInterval is how often the monitor's event loop signals that
// it is alive. The kubelet reports device health as unknown when it is not
// refreshed within each device's health check timeout (30 seconds by
// default), so the driver re-sends its health report on every heartbeat. The
// heartbeat deliberately originates from the event loop itself, after each
// NVML event wait returns: if the loop or the NVML call wedges, the resends
// stop and the kubelet correctly decays the devices' health to unknown
// instead of trusting a stale report.
const monitorHeartbeatInterval = 15 * time.Second

// eventWaitRetryDelay is how long the event loop pauses after an NVML event
// wait fails with an error other than ERROR_TIMEOUT, which return immediately
// instead of after the wait timeout. A variable so tests can shorten it.
var eventWaitRetryDelay = time.Second

type nvmlDeviceHealthMonitor struct {
	nvmllib           nvml.Interface
	eventSet          nvml.EventSet
	unhealthy         chan *DeviceHealthEvent
	perGPUAllocatable *PerGPUAllocatableDevices
	gpuInfosByUUID    map[string]*GpuInfo
	heartbeat         chan struct{}
	lastHeartbeat     time.Time
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
		heartbeat:         make(chan struct{}, 1),
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

	// The event set and the NVML session it belongs to exist only after a
	// successful RegisterEvents; a failed registration cleans up after
	// itself, so Stop must not touch NVML in that case.
	if m.eventSet != nil {
		if ret := m.eventSet.Free(); ret != nvml.SUCCESS {
			klog.Warningf("failed to unset events: %v", ret)
		}
		if ret := m.nvmllib.Shutdown(); ret != nvml.SUCCESS {
			klog.Warningf("failed to shutdown NVML: %v", ret)
		}
	}
	close(m.unhealthy)
}

func (m *nvmlDeviceHealthMonitor) run(ctx context.Context) {
	// lastRet is the previous event wait return, to log a persistent error
	// once instead of on every retry.
	lastRet := nvml.SUCCESS
	for {
		select {
		case <-ctx.Done():
			klog.V(6).Info("Stopping event-driven GPU health monitor...")
			return
		default:
			event, ret := m.eventSet.Wait(5000) // timeout in 5000 ms.

			// The heartbeat proves the NVML event stream is responsive, so
			// it fires only for returns that show NVML answered about the
			// devices: a successful wait, ERROR_TIMEOUT after the 5s deadline
			// (the normal idle path, which drives the heartbeat on a quiet,
			// healthy GPU), and ERROR_GPU_IS_LOST, where NVML is responsive
			// and reports a known failure that is tainted below, so the
			// kubelet keeps seeing the devices as unhealthy rather than
			// unknown. A wedged NVML call returns nothing at all and any
			// other error (for example ERROR_UNINITIALIZED or ERROR_UNKNOWN)
			// means no events can be received: in both cases the heartbeat
			// stops and the kubelet then reports the devices' health as
			// unknown instead of trusting stale data.
			if ret == nvml.SUCCESS || ret == nvml.ERROR_TIMEOUT || ret == nvml.ERROR_GPU_IS_LOST {
				m.beat()
			}

			if ret == nvml.ERROR_TIMEOUT {
				lastRet = ret
				continue
			}
			// Only ERROR_GPU_IS_LOST maps to a taint. Other errors are not
			// mapped to any health state: the loop backs off and retries,
			// and since no heartbeat is emitted for them (above) the health
			// reported to the kubelet decays to unknown while they persist.
			// Ref doc: [https://docs.nvidia.com/deploy/nvml-api/group__nvmlEvents.html#group__nvmlEvents_1g9714b0ca9a34c7a7780f87fee16b205c].
			if ret != nvml.SUCCESS {
				if ret == nvml.ERROR_GPU_IS_LOST {
					// A lost GPU is re-reported on every retry; warn once
					// per episode, the taint update is idempotent.
					if lastRet != ret {
						klog.Warningf("GPU is lost error: %v; Tainting all devices with %s", ret, TaintKeyGPULost)
					} else {
						klog.V(6).Infof("GPU is still lost: %v", ret)
					}
					m.sendHealthEventForAllDevices(HealthEventGPULost)
				} else {
					klog.V(6).Infof("Error waiting for NVML event: %v. Retrying...", ret)
				}
				lastRet = ret
				// A persistent error (including a GPU that stays lost)
				// returns immediately instead of after the wait timeout;
				// pause before retrying so the loop does not spin.
				select {
				case <-ctx.Done():
				case <-time.After(eventWaitRetryDelay):
				}
				continue
			}

			lastRet = ret

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
			affectedDevices, err := m.resolveDevicesByEventAddress(eventUUID, event.Device, gi, ci)
			// An error indicates inconsistent UUID/PCI inventory. No devices
			// without an error means the event's GI/CI is not available.
			if err != nil {
				klog.Warningf("Unable to resolve XID=%d event for UUID:%s, GI:%d, CI:%d: %v", xid, eventUUID, gi, ci, err)
				continue
			}
			if len(affectedDevices) == 0 {
				klog.V(6).Infof("Ignoring event for unexpected device (UUID:%s, GI:%d, CI:%d)", eventUUID, gi, ci)
				continue
			}

			for _, affectedDevice := range affectedDevices {
				klog.V(4).Infof("Sending XID=%d health event for device %s", xid, affectedDevice.CanonicalName())
			}
			// The send observes ctx so that a full channel (for example an
			// XID storm before the consumer goroutine starts) cannot park
			// this goroutine past shutdown and deadlock Stop().
			select {
			case m.unhealthy <- &DeviceHealthEvent{
				Devices:   affectedDevices,
				EventType: HealthEventXID,
				EventData: xid,
			}:
			case <-ctx.Done():
				klog.V(6).Info("Stopping event-driven GPU health monitor...")
				return
			}
		}
	}
}

func (m *nvmlDeviceHealthMonitor) Unhealthy() <-chan *DeviceHealthEvent {
	return m.unhealthy
}

// beat signals that the NVML event wait returned, at most once per
// monitorHeartbeatInterval. The interval clock only advances when the signal
// is actually delivered: a dropped beat must not consume the interval, or a
// slow consumer would stretch the gap between delivered heartbeats and erode
// the margin to the kubelet's health check timeout. Only called from the run
// goroutine.
func (m *nvmlDeviceHealthMonitor) beat() {
	if time.Since(m.lastHeartbeat) < monitorHeartbeatInterval {
		return
	}
	select {
	case m.heartbeat <- struct{}{}:
		m.lastHeartbeat = time.Now()
	default:
	}
}

// Heartbeat signals that the monitor's event loop is alive. The driver
// re-sends its current health report on each heartbeat to keep the kubelet's
// health data fresh.
func (m *nvmlDeviceHealthMonitor) Heartbeat() <-chan struct{} {
	return m.heartbeat
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
// resolveDevicesByEventAddress maps this address, extracted from an NVML
// event, to the advertised allocatable devices it affects: a MIG-scoped event
// affects that one MIG device; a full-GPU event (for example XID 79, the GPU
// fell off the bus) affects the GPU and every MIG device on it, whether the
// full GPU itself is allocatable or not (static MIG mode, Dynamic MIG on
// Ampere), as in the device plugin's health checker.
func (m *nvmlDeviceHealthMonitor) resolveDevicesByEventAddress(parentUUID string, eventDevice nvml.Device, gi, ci uint32) ([]*AllocatableDevice, error) {
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
		return devices.List(), nil

	case gi != FullGPUInstanceID && ci != FullGPUInstanceID:
		var device *AllocatableDevice
		if featuregates.Enabled(featuregates.DynamicMIG) {
			spec, err := resolveMigEvent(eventDevice, parent, gi, ci)
			if err != nil {
				return nil, fmt.Errorf("failed to resolve Dynamic MIG device for parent %s, GI=%d, CI=%d: %w", parentUUID, gi, ci, err)
			}
			device = devices.GetMigDynamicDeviceByTuple(spec)
		} else {
			device = devices.GetMigStaticDeviceByLiveTuple(&MigLiveTuple{
				ParentUUID: parentUUID,
				GIID:       int(gi),
				CIID:       int(ci),
			})
		}
		if device == nil {
			return nil, nil
		}
		return []*AllocatableDevice{device}, nil

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

// getAdditionalXids returns a list of additional Xids to skip from the specified string.
// The input is treated as a comma-separated string and all valid uint64 values are considered as Xid values.
// Invalid values are ignored.
// TODO: add list of EXPLICIT XIDs from [https://github.com/NVIDIA/k8s-device-plugin/pull/1443].
func getAdditionalXids(input string) []uint64 {
	if input == "" {
		return nil
	}

	var additionalXids []uint64
	klog.V(6).Infof("Creating a list of additional xids to ignore: [%s]", input)
	for _, additionalXid := range strings.Split(input, ",") {
		trimmed := strings.TrimSpace(additionalXid)
		if trimmed == "" {
			continue
		}
		xid, err := strconv.ParseUint(trimmed, 10, 64)
		if err != nil {
			klog.V(6).Infof("Ignoring malformed Xid value %v: %v", trimmed, err)
			continue
		}
		additionalXids = append(additionalXids, xid)
	}

	return additionalXids
}

func xidsToSkip(additionalXids string) map[uint64]bool {
	// Add the list of hardcoded disabled (ignored) XIDs:
	// https://docs.nvidia.com/deploy/xid-errors/latest/analyzing-xid-catalog.html
	// Application errors: the GPU should still be healthy.
	// If you change this list, update the documentation.
	ignoredXids := []uint64{
		13,  // Graphics Engine Exception
		31,  // GPU memory page fault
		43,  // GPU stopped processing
		45,  // Preemptive cleanup, due to previous errors
		68,  // Video processor exception
		109, // Context Switch Timeout Error
	}

	skippedXids := make(map[uint64]bool)
	for _, id := range ignoredXids {
		skippedXids[id] = true
	}

	for _, additionalXid := range getAdditionalXids(additionalXids) {
		skippedXids[additionalXid] = true
	}
	return skippedXids
}

// IsEventNonFatal evaluates whether a hardware event is considered an application-level
// warning (None) rather than a critical hardware failure (NoSchedule).
// Currently, it only checks for XID events.
func (m *nvmlDeviceHealthMonitor) IsEventNonFatal(event *DeviceHealthEvent) bool {
	if event.EventType == HealthEventXID {
		return m.skippedXids[event.EventData]
	}
	return false
}
