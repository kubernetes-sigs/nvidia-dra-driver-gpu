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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/dynamic-resource-allocation/kubeletplugin"

	"sigs.k8s.io/dra-driver-nvidia-gpu/pkg/featuregates"
)

// newHealthTestDriver builds a driver with a single full GPU device gpu-0
// (GpuInfo minor 0) and a republish stub, ready to exercise health reporting.
func newHealthTestDriver(monitor deviceHealthMonitor) (*driver, *AllocatableDevice) {
	dev := &AllocatableDevice{Gpu: &GpuInfo{}}
	d := &driver{
		nodeName:            "node1",
		deviceHealthMonitor: monitor,
		state: &DeviceState{
			perGPUAllocatable: &PerGPUAllocatableDevices{
				allocatablesMap: map[PCIBusID]AllocatableDevices{
					"0000:01:00.0": {"gpu-0": dev},
				},
			},
		},
		republishResources: func(context.Context) error { return nil },
	}
	return d, dev
}

// enableHealthGate turns the NVMLDeviceHealthCheck feature gate on for the
// duration of the test; WatchHealthStatus declines subscriptions without it.
func enableHealthGate(t *testing.T) {
	t.Helper()
	require.NoError(t, featuregates.FeatureGates().SetFromMap(map[string]bool{
		string(featuregates.NVMLDeviceHealthCheck): true,
	}))
	t.Cleanup(func() {
		require.NoError(t, featuregates.FeatureGates().SetFromMap(map[string]bool{
			string(featuregates.NVMLDeviceHealthCheck): false,
		}))
	})
}

// startWatch runs WatchHealthStatus in a goroutine, consumes and returns the
// initial snapshot, and hands back the reports channel plus a stop function
// which cancels the watch and asserts it returns cleanly.
func startWatch(t *testing.T, d *driver) (kubeletplugin.DeviceHealthReport, <-chan kubeletplugin.DeviceHealthReport, func()) {
	t.Helper()
	enableHealthGate(t)

	ctx, cancel := context.WithCancel(t.Context())
	reports := make(chan kubeletplugin.DeviceHealthReport)
	done := make(chan error, 1)
	go func() {
		done <- d.WatchHealthStatus(ctx, reports)
	}()

	var initial kubeletplugin.DeviceHealthReport
	select {
	case initial = <-reports:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for initial health report")
	}

	stop := func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("WatchHealthStatus returned error: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for WatchHealthStatus to return")
		}
	}
	return initial, reports, stop
}

// receiveReport reads one health report or fails the test.
func receiveReport(t *testing.T, reports <-chan kubeletplugin.DeviceHealthReport) kubeletplugin.DeviceHealthReport {
	t.Helper()
	select {
	case report := <-reports:
		return report
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for health report")
		return kubeletplugin.DeviceHealthReport{}
	}
}

func reportByName(report kubeletplugin.DeviceHealthReport) map[string]kubeletplugin.DeviceHealth {
	byName := map[string]kubeletplugin.DeviceHealth{}
	for _, dev := range report.Devices {
		byName[dev.DeviceName] = dev
	}
	return byName
}

func newTaint(key, value string, effect resourceapi.DeviceTaintEffect) *resourceapi.DeviceTaint {
	return &resourceapi.DeviceTaint{Key: key, Value: value, Effect: effect}
}

// TestDeviceHealthFromTaints verifies the single mapping from a device's
// taints (the health state shared with the ResourceSlice) to the health
// reported to the kubelet.
func TestDeviceHealthFromTaints(t *testing.T) {
	parent := &GpuInfo{UUID: "GPU-parent"}
	testCases := []struct {
		name            string
		dev             *AllocatableDevice
		taints          []*resourceapi.DeviceTaint
		expectedHealth  kubeletplugin.HealthStatus
		expectedMessage string
	}{
		{
			name:           "full GPU without taints",
			dev:            &AllocatableDevice{Gpu: parent},
			expectedHealth: kubeletplugin.HealthStatusHealthy,
		},
		{
			name:           "static MIG without taints",
			dev:            &AllocatableDevice{MigStatic: &MigDeviceInfo{parent: parent}},
			expectedHealth: kubeletplugin.HealthStatusHealthy,
		},
		{
			name:           "dynamic MIG without taints (GPU-scoped events fan out to it)",
			dev:            &AllocatableDevice{MigDynamic: &MigSpec{Parent: parent}},
			expectedHealth: kubeletplugin.HealthStatusHealthy,
		},
		{
			name:            "VFIO passthrough cannot be observed",
			dev:             &AllocatableDevice{Vfio: &VfioDeviceInfo{UUID: "GPU-parent", PciBusID: "0000:01:00.0"}},
			expectedHealth:  kubeletplugin.HealthStatusUnknown,
			expectedMessage: "vfio passthrough devices are not covered by the NVML health monitor",
		},
		{
			name:            "critical XID",
			dev:             &AllocatableDevice{Gpu: parent},
			taints:          []*resourceapi.DeviceTaint{newTaint(TaintKeyXID, "79", resourceapi.DeviceTaintEffectNoSchedule)},
			expectedHealth:  kubeletplugin.HealthStatusUnhealthy,
			expectedMessage: "critical XID 79 reported by NVML",
		},
		{
			name:            "non-fatal XID",
			dev:             &AllocatableDevice{Gpu: parent},
			taints:          []*resourceapi.DeviceTaint{newTaint(TaintKeyXID, "31", resourceapi.DeviceTaintEffectNone)},
			expectedHealth:  kubeletplugin.HealthStatusHealthy,
			expectedMessage: "non-fatal XID 31 reported by NVML",
		},
		{
			name:            "GPU lost",
			dev:             &AllocatableDevice{Gpu: parent},
			taints:          []*resourceapi.DeviceTaint{newTaint(TaintKeyGPULost, "", resourceapi.DeviceTaintEffectNoSchedule)},
			expectedHealth:  kubeletplugin.HealthStatusUnhealthy,
			expectedMessage: "GPU is lost",
		},
		{
			name:            "unmonitored",
			dev:             &AllocatableDevice{Gpu: parent},
			taints:          []*resourceapi.DeviceTaint{newTaint(TaintKeyUnmonitored, "", resourceapi.DeviceTaintEffectNone)},
			expectedHealth:  kubeletplugin.HealthStatusUnknown,
			expectedMessage: "device health is not monitored",
		},
		{
			name: "critical outranks unmonitored and informational",
			dev:  &AllocatableDevice{Gpu: parent},
			taints: []*resourceapi.DeviceTaint{
				newTaint(TaintKeyUnmonitored, "", resourceapi.DeviceTaintEffectNone),
				newTaint(TaintKeyGPULost, "", resourceapi.DeviceTaintEffectNoSchedule),
			},
			expectedHealth:  kubeletplugin.HealthStatusUnhealthy,
			expectedMessage: "GPU is lost",
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			for _, taint := range tc.taints {
				tc.dev.AddOrUpdateTaint(taint)
			}
			health, message := deviceHealthFromTaints(tc.dev)
			assert.Equal(t, tc.expectedHealth, health)
			assert.Equal(t, tc.expectedMessage, message)
		})
	}
}

// TestApplyQueuedHealthEvents verifies that health events queued by the
// monitor during registration (devices it could not register) are applied as
// taints before the health service is advertised, so both the initial
// ResourceSlice and an early kubelet subscription see the device as
// unmonitored.
func TestApplyQueuedHealthEvents(t *testing.T) {
	gpu0 := &AllocatableDevice{Gpu: &GpuInfo{minor: 0}}
	gpu1 := &AllocatableDevice{Gpu: &GpuInfo{minor: 1}}
	unhealthy := make(chan *DeviceHealthEvent, 1)
	unhealthy <- &DeviceHealthEvent{Devices: []*AllocatableDevice{gpu1}, EventType: HealthEventUnmonitored}
	d := &driver{
		nodeName:            "node1",
		deviceHealthMonitor: &mockHealthMonitor{unhealthyCh: unhealthy},
		state: &DeviceState{
			perGPUAllocatable: &PerGPUAllocatableDevices{
				allocatablesMap: map[PCIBusID]AllocatableDevices{
					"0000:01:00.0": {"gpu-0": gpu0},
					"0000:02:00.0": {"gpu-1": gpu1},
				},
			},
		},
	}

	d.applyQueuedHealthEvents()

	assert.Empty(t, unhealthy, "queued events must be drained")
	assert.Empty(t, gpu0.Taints())
	require.Len(t, gpu1.Taints(), 1)
	assert.Equal(t, TaintKeyUnmonitored, gpu1.Taints()[0].Key)

	initial, _, stopWatch := startWatch(t, d)
	defer stopWatch()
	byName := reportByName(initial)
	assert.Equal(t, kubeletplugin.HealthStatusHealthy, byName["gpu-0"].Health)
	assert.Equal(t, kubeletplugin.HealthStatusUnknown, byName["gpu-1"].Health)
}

// TestDeviceHealthEvents_HeartbeatResends verifies that a heartbeat from the
// NVML monitor's event loop wakes the watcher and re-sends a report with a
// fresh timestamp: the kubelet compares LastUpdated against its health check
// timeout, so a resend with a stale timestamp would still let the device
// decay to Unknown.
func TestDeviceHealthEvents_HeartbeatResends(t *testing.T) {
	monitor := &mockHealthMonitor{heartbeatCh: make(chan struct{}, 1)}
	d, _ := newHealthTestDriver(monitor)
	d.healthUpdatedAt = time.Now().Add(-time.Minute)

	initial, reports, stopWatch := startWatch(t, d)
	defer stopWatch()
	require.Len(t, initial.Devices, 1)
	stale := initial.Devices[0].LastUpdated

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	eventsDone := make(chan struct{})
	go func() {
		defer close(eventsDone)
		d.deviceHealthHeartbeats(ctx)
	}()

	before := time.Now()
	monitor.heartbeatCh <- struct{}{}

	report := receiveReport(t, reports)
	require.Len(t, report.Devices, 1)
	assert.Equal(t, "gpu-0", report.Devices[0].DeviceName)
	assert.Equal(t, kubeletplugin.HealthStatusHealthy, report.Devices[0].Health)
	assert.True(t, report.Devices[0].LastUpdated.After(stale))
	assert.False(t, report.Devices[0].LastUpdated.Before(before), "heartbeat must refresh LastUpdated")

	cancel()
	<-eventsDone
}

// TestDeviceHealthEvents_EventTaintsAndReports verifies that an event from
// the monitor taints the device, republishes the ResourceSlice once, and is
// reported to the kubelet from that same taint.
func TestDeviceHealthEvents_EventTaintsAndReports(t *testing.T) {
	monitor := &mockHealthMonitor{
		heartbeatCh: make(chan struct{}),
		unhealthyCh: make(chan *DeviceHealthEvent, 1),
	}
	d, dev := newHealthTestDriver(monitor)
	published := make(chan struct{}, 2)
	d.republishResources = func(context.Context) error {
		published <- struct{}{}
		return nil
	}

	initial, reports, stopWatch := startWatch(t, d)
	defer stopWatch()
	require.Equal(t, kubeletplugin.HealthStatusHealthy, initial.Devices[0].Health)

	ctx, cancel := context.WithCancel(t.Context())
	eventsDone := make(chan struct{})
	go func() {
		defer close(eventsDone)
		d.deviceHealthEvents(ctx)
	}()

	monitor.unhealthyCh <- &DeviceHealthEvent{
		EventType: HealthEventXID, EventData: 79, Devices: []*AllocatableDevice{dev},
	}
	select {
	case <-published:
	case <-time.After(5 * time.Second):
		t.Fatal("event was not applied")
	}
	report := receiveReport(t, reports)
	require.Len(t, report.Devices, 1)
	assert.Equal(t, kubeletplugin.HealthStatusUnhealthy, report.Devices[0].Health)
	assert.Equal(t, "critical XID 79 reported by NVML", report.Devices[0].Message)
	require.Len(t, dev.Taints(), 1)
	assert.Equal(t, resourceapi.DeviceTaintEffectNoSchedule, dev.Taints()[0].Effect)

	cancel()
	<-eventsDone
	assert.Empty(t, published, "one event must republish exactly once")
}

// TestDeviceHealth_UnhealthyIsSticky verifies that once a device is
// unhealthy, later events cannot flip it back: a non-fatal XID after a
// critical one must not mask the unrecovered failure. This follows from the
// taints being the single health state and AddOrUpdateTaint never lowering
// an effect.
func TestDeviceHealth_UnhealthyIsSticky(t *testing.T) {
	monitor := &mockHealthMonitor{nonFatalXids: map[uint64]bool{31: true}}
	d, dev := newHealthTestDriver(monitor)
	ctx := t.Context()

	d.applyHealthEventTaint(ctx, &DeviceHealthEvent{
		EventType: HealthEventXID, EventData: 79, Devices: []*AllocatableDevice{dev},
	})
	health, message := deviceHealthFromTaints(dev)
	require.Equal(t, kubeletplugin.HealthStatusUnhealthy, health)

	d.applyHealthEventTaint(ctx, &DeviceHealthEvent{
		EventType: HealthEventXID, EventData: 31, Devices: []*AllocatableDevice{dev},
	})
	health2, message2 := deviceHealthFromTaints(dev)
	assert.Equal(t, kubeletplugin.HealthStatusUnhealthy, health2)
	assert.Equal(t, message, message2, "the message must keep naming the critical failure")

	d.applyHealthEventTaint(ctx, &DeviceHealthEvent{
		EventType: HealthEventUnmonitored, Devices: []*AllocatableDevice{dev},
	})
	health3, _ := deviceHealthFromTaints(dev)
	assert.Equal(t, kubeletplugin.HealthStatusUnhealthy, health3, "unmonitored must not hide an unhealthy device")
}

// TestDeviceHealth_FollowsTaintRemoval verifies that removing a taint (as the
// Dynamic MIG unprepare path does for a destroyed instance) is reflected in
// the reported health, because both come from the same state.
func TestDeviceHealth_FollowsTaintRemoval(t *testing.T) {
	parent := &GpuInfo{UUID: "GPU-parent"}
	mig := &AllocatableDevice{MigDynamic: &MigSpec{Parent: parent}}
	mig.AddOrUpdateTaint(newTaint(TaintKeyXID, "79", resourceapi.DeviceTaintEffectNoSchedule))
	health, _ := deviceHealthFromTaints(mig)
	require.Equal(t, kubeletplugin.HealthStatusUnhealthy, health)

	_, removed := mig.RemoveTaint(TaintKeyXID)
	require.True(t, removed)
	health, message := deviceHealthFromTaints(mig)
	assert.Equal(t, kubeletplugin.HealthStatusHealthy, health)
	assert.Empty(t, message)
}

// TestNotifyHealthSubscribers_Coalesces verifies the notification semantics:
// updates arriving while a watcher is busy collapse into a single pending
// wake-up, and the report is built at send time, so the watcher sends one
// report reflecting the latest state instead of a queue of stale snapshots.
func TestNotifyHealthSubscribers_Coalesces(t *testing.T) {
	monitor := &mockHealthMonitor{nonFatalXids: map[uint64]bool{31: true}}
	d, dev := newHealthTestDriver(monitor)
	ctx := t.Context()

	// Register a subscriber directly, without a running watcher, to model a
	// watcher which is busy while updates arrive.
	subscriber := make(chan struct{}, 1)
	d.healthSubMu.Lock()
	d.healthSubscribers = append(d.healthSubscribers, subscriber)
	d.healthSubMu.Unlock()

	// A burst of updates and a heartbeat while the watcher is busy.
	require.True(t, d.applyHealthEventTaint(ctx, &DeviceHealthEvent{
		EventType: HealthEventXID, EventData: 31, Devices: []*AllocatableDevice{dev},
	}))
	d.deviceHealthChanged()
	require.True(t, d.applyHealthEventTaint(ctx, &DeviceHealthEvent{
		EventType: HealthEventXID, EventData: 79, Devices: []*AllocatableDevice{dev},
	}))
	d.deviceHealthChanged()
	d.deviceHealthConfirmed()

	// Exactly one pending wake-up, no queued snapshots.
	require.Len(t, subscriber, 1)

	// What the watcher does on wake-up: build the report at send time. It
	// must reflect the latest state (the critical XID), not the first event
	// of the burst.
	<-subscriber
	report := d.buildHealthReport()
	require.Len(t, report.Devices, 1)
	assert.Equal(t, kubeletplugin.HealthStatusUnhealthy, report.Devices[0].Health)
	assert.Contains(t, report.Devices[0].Message, "critical XID 79")

	// Nothing left pending: the burst coalesced into that single wake-up.
	assert.Empty(t, subscriber)
}

// TestBuildHealthReport_TracksDevices verifies that each report reflects the
// current allocatable devices (for example a VFIO sibling removed and
// re-added at unprepare), and that a report never waits behind the state
// lock: while a claim operation holds it, the previous report is re-sent
// with a fresh timestamp.
func TestBuildHealthReport_TracksDevices(t *testing.T) {
	d, _ := newHealthTestDriver(&mockHealthMonitor{})

	d.state.Lock()
	d.state.perGPUAllocatable.allocatablesMap["0000:02:00.0"] = AllocatableDevices{
		"gpu-1": {Gpu: &GpuInfo{}},
	}
	d.state.Unlock()
	report := d.buildHealthReport()
	require.Len(t, report.Devices, 2)
	assert.Equal(t, kubeletplugin.HealthStatusHealthy, reportByName(report)["gpu-1"].Health)

	d.state.Lock()
	delete(d.state.perGPUAllocatable.allocatablesMap, "0000:01:00.0")
	d.state.Unlock()
	report = d.buildHealthReport()
	require.Len(t, report.Devices, 1)
	assert.Equal(t, "gpu-1", report.Devices[0].DeviceName)
	first := report.Devices[0].LastUpdated

	// State lock held by a claim operation which is changing the device
	// set: the report must come back at once with the previous snapshot
	// (not the half-modified map) and a fresh timestamp.
	d.state.Lock()
	d.state.perGPUAllocatable.allocatablesMap["0000:03:00.0"] = AllocatableDevices{
		"gpu-2": {Gpu: &GpuInfo{}},
	}
	d.touchDeviceHealth()
	done := make(chan kubeletplugin.DeviceHealthReport, 1)
	go func() { done <- d.buildHealthReport() }()
	select {
	case report = <-done:
	case <-time.After(2 * time.Second):
		d.state.Unlock()
		t.Fatal("buildHealthReport blocked on the state lock")
	}
	d.state.Unlock()
	require.Len(t, report.Devices, 1)
	assert.Equal(t, "gpu-1", report.Devices[0].DeviceName)
	assert.True(t, report.Devices[0].LastUpdated.After(first))

	// Lock released: the next report picks up the change.
	report = d.buildHealthReport()
	require.Len(t, report.Devices, 2)
	assert.Contains(t, reportByName(report), "gpu-2")
}

// runHealthConsumers starts the driver's health consumer goroutines the way
// NewDriver does and returns a function that stops them.
func runHealthConsumers(t *testing.T, d *driver) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	d.startDeviceHealthConsumers(ctx)
	return func() {
		cancel()
		done := make(chan struct{})
		go func() {
			defer close(done)
			d.wg.Wait()
		}()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for health consumers to stop")
		}
	}
}

// expectNoReport asserts that no health report arrives within d.
func expectNoReport(t *testing.T, reports <-chan kubeletplugin.DeviceHealthReport, d time.Duration, msg string) {
	t.Helper()
	select {
	case <-reports:
		t.Fatal(msg)
	case <-time.After(d):
	}
}

// TestHeartbeatResend_NotStarvedByStateLock verifies that a heartbeat still
// triggers a re-send while the event consumer is parked on the state lock
// (a health event arrived during a long Prepare/Unprepare). If it did not,
// the kubelet would decay every device to Unknown while NVML is answering.
func TestHeartbeatResend_NotStarvedByStateLock(t *testing.T) {
	monitor := &mockHealthMonitor{
		heartbeatCh: make(chan struct{}, 1),
		unhealthyCh: make(chan *DeviceHealthEvent, 1),
	}
	d, dev := newHealthTestDriver(monitor)
	_, reports, stopWatch := startWatch(t, d)
	defer stopWatch()

	stopConsumers := runHealthConsumers(t, d)
	defer stopConsumers()

	// A claim operation holds the state lock for a long time.
	d.state.Lock()
	unlocked := false
	defer func() {
		if !unlocked {
			d.state.Unlock()
		}
	}()

	// An event arrives; the consumer dequeues it and blocks in AddDeviceTaint.
	monitor.unhealthyCh <- &DeviceHealthEvent{
		EventType: HealthEventXID, EventData: 79, Devices: []*AllocatableDevice{dev},
	}
	require.Eventually(t, func() bool { return len(monitor.unhealthyCh) == 0 }, 5*time.Second, 5*time.Millisecond)

	// The monitor keeps beating: the report must still be re-sent (the
	// previous snapshot with a fresh timestamp), the lock must not matter.
	monitor.heartbeatCh <- struct{}{}
	select {
	case report := <-reports:
		require.Len(t, report.Devices, 1)
		assert.Equal(t, kubeletplugin.HealthStatusHealthy, report.Devices[0].Health, "previous snapshot is re-sent while the lock is busy")
	case <-time.After(2 * time.Second):
		t.Fatal("heartbeat re-send was starved by the state lock")
	}

	// Release the lock: the event is applied and reported.
	d.state.Unlock()
	unlocked = true
	require.Eventually(t, func() bool {
		select {
		case report := <-reports:
			return report.Devices[0].Health == kubeletplugin.HealthStatusUnhealthy
		default:
			return false
		}
	}, 5*time.Second, 5*time.Millisecond, "event must be reported once the lock is released")
}

// TestWatchHealthStatus_InitialReportWhileStateLockBusy verifies that the
// initial report covers all devices even when a claim operation holds the
// state lock at subscription time (the DRAPlugin contract requires an
// initial report covering all devices).
func TestWatchHealthStatus_InitialReportWhileStateLockBusy(t *testing.T) {
	d, _ := newHealthTestDriver(&mockHealthMonitor{})
	// NewDriver seeds the snapshot before the kubelet can subscribe.
	d.refreshDeviceHealth()

	d.state.Lock()
	initial, _, stopWatch := startWatch(t, d)
	d.state.Unlock()
	defer stopWatch()

	require.Len(t, initial.Devices, 1, "initial report must cover all devices even while the state lock is busy")
	assert.Equal(t, "gpu-0", initial.Devices[0].DeviceName)
}

// TestDeviceHealth_TaintChangeReportedWhileStateLockBusy verifies that a
// taint applied by the event consumer is reflected in the next report even
// if a claim operation grabs the state lock before the watcher wakes up. A
// re-sent snapshot must never carry a fresh timestamp for stale health.
func TestDeviceHealth_TaintChangeReportedWhileStateLockBusy(t *testing.T) {
	d, dev := newHealthTestDriver(&mockHealthMonitor{})
	require.Equal(t, kubeletplugin.HealthStatusHealthy, d.buildHealthReport().Devices[0].Health)

	// What the event consumer does for an event.
	require.True(t, d.applyHealthEventTaint(t.Context(), &DeviceHealthEvent{
		EventType: HealthEventXID, EventData: 79, Devices: []*AllocatableDevice{dev},
	}))
	d.deviceHealthChanged()

	// A claim operation takes the lock before the watcher builds its report.
	d.state.Lock()
	defer d.state.Unlock()
	report := d.buildHealthReport()
	require.Len(t, report.Devices, 1)
	assert.Equal(t, kubeletplugin.HealthStatusUnhealthy, report.Devices[0].Health, "applied taint must be reported even while the state lock is busy")
}

// TestDeviceHealthEvents_RepeatedEventDoesNotResend verifies that an event
// which does not change any taint (a persistently lost GPU re-reports every
// retry) neither republishes the ResourceSlice nor wakes the health watchers.
func TestDeviceHealthEvents_RepeatedEventDoesNotResend(t *testing.T) {
	monitor := &mockHealthMonitor{
		heartbeatCh: make(chan struct{}),
		unhealthyCh: make(chan *DeviceHealthEvent, 1),
	}
	d, dev := newHealthTestDriver(monitor)
	published := make(chan struct{}, 4)
	d.republishResources = func(context.Context) error {
		published <- struct{}{}
		return nil
	}
	_, reports, stopWatch := startWatch(t, d)
	defer stopWatch()
	stopConsumers := runHealthConsumers(t, d)
	defer stopConsumers()

	event := func() *DeviceHealthEvent {
		return &DeviceHealthEvent{EventType: HealthEventGPULost, Devices: []*AllocatableDevice{dev}}
	}

	monitor.unhealthyCh <- event()
	report := receiveReport(t, reports)
	assert.Equal(t, kubeletplugin.HealthStatusUnhealthy, report.Devices[0].Health)
	require.Len(t, published, 1)

	monitor.unhealthyCh <- event()
	require.Eventually(t, func() bool { return len(monitor.unhealthyCh) == 0 }, 5*time.Second, 5*time.Millisecond)
	expectNoReport(t, reports, 200*time.Millisecond, "a repeated event with no taint change must not re-send the health report")
	assert.Len(t, published, 1, "a repeated event with no taint change must not republish")
}
