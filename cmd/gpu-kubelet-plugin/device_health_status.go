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
	"slices"
	"time"

	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/dynamic-resource-allocation/kubeletplugin"
	"k8s.io/klog/v2"

	"sigs.k8s.io/dra-driver-nvidia-gpu/pkg/featuregates"
)

// This file implements device health reporting (KEP-4680) for the GPU kubelet
// plugin: the version-neutral [kubeletplugin.DRAPlugin] WatchHealthStatus
// API, through which the health of allocated GPUs surfaces in
// pod.status.containerStatuses[].allocatedResourcesStatus.
//
// There is exactly one health state per device: the DRA device taints
// (KEP-5055) that the NVML health monitor's events put on each
// AllocatableDevice and that are published in the ResourceSlice. The health
// reported to the kubelet is derived from those taints at report time
// (deviceHealthFromTaints), so the scheduler-facing and the pod-facing view of
// a device can never disagree: whatever makes a taint sticky, or removes it
// (a destroyed dynamic MIG device, a re-discovered VFIO sibling), applies to
// both.

// deviceHealthFromTaints derives the kubelet-facing health of a device from
// its taints:
//
//   - a NoSchedule health taint (critical XID, GPU lost) makes it Unhealthy;
//   - an unmonitored taint makes it Unknown;
//   - an informational XID taint keeps it Healthy but surfaces the event;
//   - without health taints it is Healthy, except for VFIO passthrough
//     devices, which NVML cannot observe and which are therefore Unknown.
//
// The taints are sticky in the critical direction (AddOrUpdateTaint never
// lowers an effect), so once a device is Unhealthy a later non-fatal event
// does not mask the unrecovered failure. Recovery requires fixing the device
// and restarting the driver, which re-discovers the device without taints.
func deviceHealthFromTaints(dev *AllocatableDevice) (kubeletplugin.HealthStatus, string) {
	var unmonitored, informational *resourceapi.DeviceTaint
	taints := dev.Taints()
	for i := range taints {
		taint := &taints[i]
		switch taint.Key {
		case TaintKeyGPULost:
			if taint.Effect != resourceapi.DeviceTaintEffectNone {
				return kubeletplugin.HealthStatusUnhealthy, "GPU is lost"
			}
		case TaintKeyXID:
			if taint.Effect != resourceapi.DeviceTaintEffectNone {
				return kubeletplugin.HealthStatusUnhealthy, fmt.Sprintf("critical XID %s reported by NVML", taint.Value)
			}
			informational = taint
		case TaintKeyUnmonitored:
			unmonitored = taint
		}
	}
	switch {
	case unmonitored != nil:
		return kubeletplugin.HealthStatusUnknown, "device health is not monitored"
	case informational != nil:
		return kubeletplugin.HealthStatusHealthy, fmt.Sprintf("non-fatal XID %s reported by NVML", informational.Value)
	case dev.Type() == VfioDeviceType:
		return kubeletplugin.HealthStatusUnknown, "vfio passthrough devices are not covered by the NVML health monitor"
	default:
		return kubeletplugin.HealthStatusHealthy, ""
	}
}

// applyQueuedHealthEvents applies the health events the NVML monitor queued
// during event registration (devices it could not register are reported as
// unmonitored) as device taints, before the health service is advertised and
// before the initial ResourceSlice publish. An early kubelet subscription
// therefore never sees such a device as Healthy, and the initial slices carry
// the taints without a republish.
func (d *driver) applyQueuedHealthEvents() {
	if d.deviceHealthMonitor == nil {
		return
	}
	for {
		select {
		case event, ok := <-d.deviceHealthMonitor.Unhealthy():
			if !ok {
				return
			}
			event.logIfUnknownType()
			taint := healthEventToTaint(d.deviceHealthMonitor, event)
			for _, dev := range event.Devices {
				klog.Warningf("Received %s health event for device %s during startup", event.EventType, dev.CanonicalName())
				d.state.AddDeviceTaint(dev, taint)
			}
		default:
			return
		}
	}
}

// deviceHealthChanged is called after the device taints or the device set
// changed (a health event was applied, a taint was removed, a VFIO sibling
// re-discovered): it refreshes the health snapshot from the devices and wakes
// the WatchHealthStatus subscribers so they re-send it. The refresh waits for
// the state lock: the callers have just modified the devices under that lock
// themselves, so it is free or about to be, and the snapshot must reflect the
// change before the subscribers wake, whatever claim operation runs next.
func (d *driver) deviceHealthChanged() {
	d.refreshDeviceHealth()
	d.notifyHealthSubscribers()
}

// deviceHealthConfirmed is called on each heartbeat of the NVML monitor's
// event loop: the health of every device is confirmed as current, and the
// subscribers re-send it with a fresh timestamp. (The kubelet stamps its own
// receipt time on each report; the timestamp is kept fresh for the kubelet
// plugin helper and for readers of the stream.)
func (d *driver) deviceHealthConfirmed() {
	d.touchDeviceHealth()
	d.notifyHealthSubscribers()
}

func (d *driver) touchDeviceHealth() {
	d.healthMu.Lock()
	d.healthUpdatedAt = time.Now()
	d.healthMu.Unlock()
}

// refreshDeviceHealth rebuilds the health snapshot from the current device
// taints, waiting for the state lock if a claim operation holds it. NewDriver
// calls it once before the kubelet can subscribe, so that the initial report
// always covers all devices; deviceHealthChanged calls it after every taint
// or device set change.
func (d *driver) refreshDeviceHealth() {
	d.state.Lock()
	entries := d.snapshotDeviceHealthLocked()
	d.state.Unlock()

	d.healthMu.Lock()
	d.lastHealthReport = entries
	d.healthMu.Unlock()
}

// snapshotDeviceHealthLocked derives the health of every allocatable device
// from its taints. The state lock must be held.
func (d *driver) snapshotDeviceHealthLocked() []kubeletplugin.DeviceHealth {
	allocatable := d.state.perGPUAllocatable.GetAllDevices()
	entries := make([]kubeletplugin.DeviceHealth, 0, len(allocatable))
	for devname, dev := range allocatable {
		health, message := deviceHealthFromTaints(dev)
		entries = append(entries, kubeletplugin.DeviceHealth{
			PoolName:   d.nodeName,
			DeviceName: devname,
			Health:     health,
			Message:    message,
		})
	}
	return entries
}

// buildHealthReport returns the health of every allocatable device, derived
// from its taints. The allocatable device maps are mutated at claim prepare
// and unprepare time under the state lock, which those operations hold for
// their whole duration. A report must never wait behind a slow prepare, or
// the kubelet's health check timeout expires and every device decays to
// Unknown while NVML is fine: when the lock is busy the last snapshot is
// re-sent with a fresh timestamp. That snapshot is never stale: every taint
// or device set change refreshes it (deviceHealthChanged) before the
// subscribers are woken, and NewDriver seeds it before the kubelet can
// subscribe.
func (d *driver) buildHealthReport() kubeletplugin.DeviceHealthReport {
	d.healthMu.Lock()
	defer d.healthMu.Unlock()

	if d.healthUpdatedAt.IsZero() {
		d.healthUpdatedAt = time.Now()
	}

	if d.state.TryLock() {
		d.lastHealthReport = d.snapshotDeviceHealthLocked()
		d.state.Unlock()
	} else {
		klog.V(6).Info("Device state busy; re-sending the previous health report")
	}

	devices := make([]kubeletplugin.DeviceHealth, len(d.lastHealthReport))
	for i, entry := range d.lastHealthReport {
		entry.LastUpdated = d.healthUpdatedAt
		devices[i] = entry
	}
	return kubeletplugin.DeviceHealthReport{Devices: devices}
}

// notifyHealthSubscribers wakes all pending WatchHealthStatus calls to send a
// fresh health report. The subscriber channels have capacity one and the send
// is non-blocking, so notifications coalesce: however many updates arrive
// while a subscriber is busy, it wakes once and builds the latest snapshot at
// send time. No health data is queued here; the state lives on the devices
// and this is only a notification.
func (d *driver) notifyHealthSubscribers() {
	d.healthSubMu.RLock()
	defer d.healthSubMu.RUnlock()

	for _, subscriber := range d.healthSubscribers {
		select {
		case subscriber <- struct{}{}:
		default:
		}
	}
}

// WatchHealthStatus implements [kubeletplugin.DRAPlugin]. The kubeletplugin
// helper calls it whenever the kubelet subscribes to device health updates and
// takes care of translating the reports into the DRAResourceHealth gRPC API
// version that the kubelet supports.
func (d *driver) WatchHealthStatus(ctx context.Context, reports chan<- kubeletplugin.DeviceHealthReport) error {
	if !featuregates.Enabled(featuregates.NVMLDeviceHealthCheck) {
		// The health service is not advertised in this case (see the
		// HealthService option in NewDriver), so the kubelet is not expected
		// to subscribe at all; answer any stray subscription accordingly.
		return kubeletplugin.ErrHealthNotSupported
	}

	klog.V(4).Info("Kubelet subscribed to device health updates")

	// A capacity-one notification channel: notifications coalesce, and the
	// report is built fresh at send time (see notifyHealthSubscribers).
	subscriber := make(chan struct{}, 1)
	d.healthSubMu.Lock()
	d.healthSubscribers = append(d.healthSubscribers, subscriber)
	d.healthSubMu.Unlock()

	defer func() {
		d.healthSubMu.Lock()
		d.healthSubscribers = slices.DeleteFunc(d.healthSubscribers, func(ch chan struct{}) bool {
			return ch == subscriber
		})
		d.healthSubMu.Unlock()
		klog.V(4).Info("Kubelet unsubscribed from device health updates")
	}()

	select {
	case <-ctx.Done():
		return nil
	case reports <- d.buildHealthReport():
	}

	// Send a fresh snapshot on every notification. Periodic resends
	// (required because the kubelet treats health data older than the
	// health check timeout as stale) are not generated here: the
	// notifications are driven by the NVML monitor's heartbeat, so that a
	// resend is evidence the monitoring loop is actually alive.
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-subscriber:
			// Empty reports (a node with no allocatable devices) are sent
			// too: they are valid subset reports for the kubelet, and the
			// kubeletplugin helper resets its staleness tracking only when
			// it receives a report, so suppressing them would trigger
			// spurious staleness errors on device-less nodes.
			select {
			case <-ctx.Done():
				return nil
			case reports <- d.buildHealthReport():
			}
		}
	}
}
