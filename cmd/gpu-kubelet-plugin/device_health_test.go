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
	"testing"

	nvdev "github.com/NVIDIA/go-nvlib/pkg/nvlib/device"
	"github.com/NVIDIA/go-nvml/pkg/nvml"
	resourceapi "k8s.io/api/resource/v1"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type mockNVMLDevice struct {
	nvml.Device
	getFieldValuesFunc  func([]nvml.FieldValue) nvml.Return
	getFieldValuesCalls int
}

func (m *mockNVMLDevice) GetFieldValues(values []nvml.FieldValue) nvml.Return {
	m.getFieldValuesCalls++
	return m.getFieldValuesFunc(values)
}

type mockNVMLLibrary struct {
	nvml.Interface
	deviceGetHandleByPciBusIdFunc  func(string) (nvml.Device, nvml.Return)
	deviceGetHandleByPciBusIdCalls int
}

func (m *mockNVMLLibrary) DeviceGetHandleByPciBusId(busID string) (nvml.Device, nvml.Return) {
	m.deviceGetHandleByPciBusIdCalls++
	return m.deviceGetHandleByPciBusIdFunc(busID)
}

// mockHealthMonitor implements deviceHealthMonitor for testing healthEventToTaint.
type mockHealthMonitor struct {
	nonFatalXids map[uint64]bool
}

func (m *mockHealthMonitor) Start(context.Context) error          { return nil }
func (m *mockHealthMonitor) Stop()                                {}
func (m *mockHealthMonitor) Unhealthy() <-chan *DeviceHealthEvent { return nil }
func (m *mockHealthMonitor) IsEventNonFatal(e *DeviceHealthEvent) bool {
	if e.EventType == HealthEventXID {
		return m.nonFatalXids[e.EventData]
	}
	return false
}

func TestAddOrUpdateTaint_NewTaint(t *testing.T) {
	dev := &AllocatableDevice{}
	taint := &resourceapi.DeviceTaint{
		Key:    TaintKeyXID,
		Value:  "48",
		Effect: resourceapi.DeviceTaintEffectNoSchedule,
	}

	changed := dev.AddOrUpdateTaint(taint)

	require.True(t, changed)
	require.Len(t, dev.Taints(), 1)
	assert.Equal(t, TaintKeyXID, dev.Taints()[0].Key)
	assert.Equal(t, "48", dev.Taints()[0].Value)
	assert.Equal(t, resourceapi.DeviceTaintEffectNoSchedule, dev.Taints()[0].Effect)
}

func TestAddOrUpdateTaint_DuplicateNoChange(t *testing.T) {
	dev := &AllocatableDevice{}
	taint := &resourceapi.DeviceTaint{
		Key:    TaintKeyGPULost,
		Effect: resourceapi.DeviceTaintEffectNoSchedule,
	}

	dev.AddOrUpdateTaint(taint)
	changed := dev.AddOrUpdateTaint(taint)

	assert.False(t, changed, "identical taint should not count as a change")
	assert.Len(t, dev.Taints(), 1)
}

func TestAddOrUpdateTaint_NoScheduleIsSticky(t *testing.T) {
	tests := []struct {
		name     string
		incoming resourceapi.DeviceTaint
	}{
		{
			name: "later NoSchedule XID",
			incoming: resourceapi.DeviceTaint{
				Key:    TaintKeyXID,
				Value:  "63",
				Effect: resourceapi.DeviceTaintEffectNoSchedule,
			},
		},
		{
			name: "later informational XID",
			incoming: resourceapi.DeviceTaint{
				Key:    TaintKeyXID,
				Value:  "43",
				Effect: resourceapi.DeviceTaintEffectNone,
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			original := resourceapi.DeviceTaint{
				Key:    TaintKeyXID,
				Value:  "48",
				Effect: resourceapi.DeviceTaintEffectNoSchedule,
			}
			dev := &AllocatableDevice{}
			require.True(t, dev.AddOrUpdateTaint(&original))

			assert.False(t, dev.AddOrUpdateTaint(&tc.incoming))
			assert.Equal(t, []resourceapi.DeviceTaint{original}, dev.Taints())
		})
	}
}

func TestAddOrUpdateTaint_UpdateEffect(t *testing.T) {
	dev := &AllocatableDevice{}
	dev.AddOrUpdateTaint(&resourceapi.DeviceTaint{
		Key:    TaintKeyXID,
		Value:  "48",
		Effect: resourceapi.DeviceTaintEffectNone,
	})

	changed := dev.AddOrUpdateTaint(&resourceapi.DeviceTaint{
		Key:    TaintKeyXID,
		Value:  "48",
		Effect: resourceapi.DeviceTaintEffectNoSchedule,
	})

	require.True(t, changed)
	assert.Equal(t, resourceapi.DeviceTaintEffectNoSchedule, dev.Taints()[0].Effect)
}

func TestAddOrUpdateTaint_DifferentKeysAppended(t *testing.T) {
	dev := &AllocatableDevice{}
	dev.AddOrUpdateTaint(&resourceapi.DeviceTaint{
		Key:    TaintKeyXID,
		Value:  "48",
		Effect: resourceapi.DeviceTaintEffectNoSchedule,
	})
	dev.AddOrUpdateTaint(&resourceapi.DeviceTaint{
		Key:    TaintKeyGPULost,
		Effect: resourceapi.DeviceTaintEffectNoSchedule,
	})

	taints := dev.Taints()
	require.Len(t, taints, 2)
	assert.Equal(t, TaintKeyXID, taints[0].Key)
	assert.Equal(t, TaintKeyGPULost, taints[1].Key)
}

func TestAddOrUpdateTaint_TimeAddedResetOnChange(t *testing.T) {
	dev := &AllocatableDevice{}
	dev.AddOrUpdateTaint(&resourceapi.DeviceTaint{
		Key:    TaintKeyXID,
		Value:  "48",
		Effect: resourceapi.DeviceTaintEffectNone,
	})

	dev.AddOrUpdateTaint(&resourceapi.DeviceTaint{
		Key:    TaintKeyXID,
		Value:  "63",
		Effect: resourceapi.DeviceTaintEffectNoSchedule,
	})

	assert.Nil(t, dev.Taints()[0].TimeAdded, "TimeAdded should be nil so the API server sets a fresh timestamp")
}

func TestPartGetDeviceIncludesHealthTaints(t *testing.T) {
	parent := &GpuInfo{
		UUID:                  "GPU-parent-1",
		minor:                 0,
		cudaComputeCapability: "9.0",
		driverVersion:         "580.0",
		cudaDriverVersion:     "13.0",
	}
	dev := &AllocatableDevice{MigDynamic: &MigSpec{
		Parent:        parent,
		Profile:       &nvdev.MigProfileInfo{G: 1, GB: 5, GIProfileID: 19},
		GIProfileInfo: nvml.GpuInstanceProfileInfo{Id: 19},
		Placement:     nvml.GpuInstancePlacement{Start: 0, Size: 1},
	}}
	taint := &resourceapi.DeviceTaint{
		Key:    TaintKeyXID,
		Value:  "43",
		Effect: resourceapi.DeviceTaintEffectNone,
	}
	require.True(t, dev.AddOrUpdateTaint(taint))

	got := dev.PartGetDevice(nil)
	require.Len(t, got.Taints, 1)
	assert.Equal(t, *taint, got.Taints[0])
}

func TestHealthEventToTaint(t *testing.T) {
	monitor := &mockHealthMonitor{
		nonFatalXids: map[uint64]bool{13: true, 31: true},
	}

	tests := []struct {
		name           string
		event          *DeviceHealthEvent
		monitor        deviceHealthMonitor
		expectedKey    string
		expectedValue  string
		expectedEffect resourceapi.DeviceTaintEffect
	}{
		{
			name: "fatal XID",
			event: &DeviceHealthEvent{
				EventType: HealthEventXID,
				EventData: 48,
			},
			monitor:        monitor,
			expectedKey:    TaintKeyXID,
			expectedValue:  "48",
			expectedEffect: resourceapi.DeviceTaintEffectNoSchedule,
		},
		{
			name: "non-fatal XID (skipped)",
			event: &DeviceHealthEvent{
				EventType: HealthEventXID,
				EventData: 13,
			},
			monitor:        monitor,
			expectedKey:    TaintKeyXID,
			expectedValue:  "13",
			expectedEffect: resourceapi.DeviceTaintEffectNone,
		},
		{
			name: "XID with nil monitor defaults to fatal",
			event: &DeviceHealthEvent{
				EventType: HealthEventXID,
				EventData: 13,
			},
			monitor:        nil,
			expectedKey:    TaintKeyXID,
			expectedValue:  "13",
			expectedEffect: resourceapi.DeviceTaintEffectNoSchedule,
		},
		{
			name: "GPU lost",
			event: &DeviceHealthEvent{
				EventType: HealthEventGPULost,
			},
			monitor:        monitor,
			expectedKey:    TaintKeyGPULost,
			expectedValue:  "",
			expectedEffect: resourceapi.DeviceTaintEffectNoSchedule,
		},
		{
			name: "unmonitored",
			event: &DeviceHealthEvent{
				EventType: HealthEventUnmonitored,
			},
			monitor:        monitor,
			expectedKey:    TaintKeyUnmonitored,
			expectedValue:  "",
			expectedEffect: resourceapi.DeviceTaintEffectNone,
		},
		{
			name: "unknown event type defaults to unmonitored",
			event: &DeviceHealthEvent{
				EventType: DeviceHealthEventType("bogus"),
			},
			monitor:        monitor,
			expectedKey:    TaintKeyUnmonitored,
			expectedValue:  "",
			expectedEffect: resourceapi.DeviceTaintEffectNone,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			taint := healthEventToTaint(tc.monitor, tc.event)
			assert.Equal(t, tc.expectedKey, taint.Key)
			assert.Equal(t, tc.expectedValue, taint.Value)
			assert.Equal(t, tc.expectedEffect, taint.Effect)
		})
	}
}

func newMockRecoveryActionDevice(
	t *testing.T,
	action nvml.DeviceGpuRecoveryAction,
	queryRet nvml.Return,
	fieldRet nvml.Return,
	valueType nvml.ValueType,
) *mockNVMLDevice {
	t.Helper()

	return &mockNVMLDevice{
		getFieldValuesFunc: func(values []nvml.FieldValue) nvml.Return {
			require.Len(t, values, 1)
			require.EqualValues(t, nvml.FI_DEV_GET_GPU_RECOVERY_ACTION, values[0].FieldId)

			if queryRet != nvml.SUCCESS {
				return queryRet
			}

			values[0].NvmlReturn = uint32(fieldRet)
			values[0].ValueType = uint32(valueType)
			binary.NativeEndian.PutUint32(values[0].Value[:4], uint32(action))
			return nvml.SUCCESS
		},
	}
}

func TestXidsToSkip(t *testing.T) {
	tests := map[string]struct {
		input string
		want  map[uint64]bool
	}{
		"empty input has no built-in defaults": {
			want: map[uint64]bool{},
		},
		"configured XIDs": {
			input: "13, 109",
			want:  map[uint64]bool{13: true, 109: true},
		},
		"malformed empty and duplicate values": {
			input: "43,invalid,,43",
			want:  map[uint64]bool{43: true},
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tc.want, xidsToSkip(tc.input))
		})
	}
}

func TestQueryGPURecoveryAction(t *testing.T) {
	tests := map[string]struct {
		action    nvml.DeviceGpuRecoveryAction
		queryRet  nvml.Return
		fieldRet  nvml.Return
		valueType nvml.ValueType
		want      nvml.DeviceGpuRecoveryAction
		wantErr   string
	}{
		"none": {
			action:    nvml.GPU_RECOVERY_ACTION_NONE,
			fieldRet:  nvml.SUCCESS,
			valueType: nvml.VALUE_TYPE_UNSIGNED_INT,
			want:      nvml.GPU_RECOVERY_ACTION_NONE,
		},
		"GPU reset": {
			action:    nvml.GPU_RECOVERY_ACTION_GPU_RESET,
			fieldRet:  nvml.SUCCESS,
			valueType: nvml.VALUE_TYPE_UNSIGNED_INT,
			want:      nvml.GPU_RECOVERY_ACTION_GPU_RESET,
		},
		"recover IMEX domain": {
			action:    nvml.GPU_RECOVERY_ACTION_RECOVER_IMEX_DOMAIN,
			fieldRet:  nvml.SUCCESS,
			valueType: nvml.VALUE_TYPE_UNSIGNED_INT,
			want:      nvml.GPU_RECOVERY_ACTION_RECOVER_IMEX_DOMAIN,
		},
		"unknown action": {
			action:    nvml.DeviceGpuRecoveryAction(99),
			fieldRet:  nvml.SUCCESS,
			valueType: nvml.VALUE_TYPE_UNSIGNED_INT,
			want:      nvml.DeviceGpuRecoveryAction(99),
		},
		"query failure": {
			queryRet: nvml.ERROR_UNKNOWN,
			wantErr:  "failed to query GPU recovery action",
		},
		"field failure": {
			fieldRet:  nvml.ERROR_NOT_SUPPORTED,
			valueType: nvml.VALUE_TYPE_UNSIGNED_INT,
			wantErr:   "failed to read GPU recovery action field",
		},
		"unexpected value type": {
			fieldRet:  nvml.SUCCESS,
			valueType: nvml.VALUE_TYPE_DOUBLE,
			wantErr:   "failed to decode GPU recovery action",
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			device := newMockRecoveryActionDevice(t, tc.action, tc.queryRet, tc.fieldRet, tc.valueType)

			got, err := queryGPURecoveryAction(device)
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				assert.Equal(t, 1, device.getFieldValuesCalls)
				return
			}

			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
			assert.Equal(t, 1, device.getFieldValuesCalls)
		})
	}
}

func TestIsEventNonFatal(t *testing.T) {
	const (
		pciBusID = "0000:01:00.0"
		xid      = uint64(43)
	)

	tests := map[string]struct {
		eventType DeviceHealthEventType
		action    nvml.DeviceGpuRecoveryAction
		ignored   bool
		mig       bool
		handleRet nvml.Return
		queryRet  nvml.Return
		fieldRet  nvml.Return
		want      bool
	}{
		"recovery action none is non-fatal": {
			eventType: HealthEventXID,
			action:    nvml.GPU_RECOVERY_ACTION_NONE,
			want:      true,
		},
		"MIG event queries the parent GPU": {
			eventType: HealthEventXID,
			action:    nvml.GPU_RECOVERY_ACTION_NONE,
			mig:       true,
			want:      true,
		},
		"GPU reset is fatal": {
			eventType: HealthEventXID,
			action:    nvml.GPU_RECOVERY_ACTION_GPU_RESET,
		},
		"node reboot is fatal": {
			eventType: HealthEventXID,
			action:    nvml.GPU_RECOVERY_ACTION_NODE_REBOOT,
		},
		"drain P2P is fatal": {
			eventType: HealthEventXID,
			action:    nvml.GPU_RECOVERY_ACTION_DRAIN_P2P,
		},
		"drain and reset is fatal": {
			eventType: HealthEventXID,
			action:    nvml.GPU_RECOVERY_ACTION_DRAIN_AND_RESET,
		},
		"recover IMEX domain is non-fatal for GPU scheduling": {
			eventType: HealthEventXID,
			action:    nvml.GPU_RECOVERY_ACTION_RECOVER_IMEX_DOMAIN,
			want:      true,
		},
		"unknown non-zero action is fatal": {
			eventType: HealthEventXID,
			action:    nvml.DeviceGpuRecoveryAction(99),
		},
		"administrator override remains non-fatal": {
			eventType: HealthEventXID,
			action:    nvml.GPU_RECOVERY_ACTION_GPU_RESET,
			ignored:   true,
			want:      true,
		},
		"query failure is fatal": {
			eventType: HealthEventXID,
			queryRet:  nvml.ERROR_UNKNOWN,
		},
		"administrator override survives query failure": {
			eventType: HealthEventXID,
			ignored:   true,
			queryRet:  nvml.ERROR_UNKNOWN,
			want:      true,
		},
		"field failure is fatal": {
			eventType: HealthEventXID,
			fieldRet:  nvml.ERROR_NOT_SUPPORTED,
		},
		"parent handle failure is fatal": {
			eventType: HealthEventXID,
			handleRet: nvml.ERROR_INVALID_ARGUMENT,
		},
		"administrator override survives parent handle failure": {
			eventType: HealthEventXID,
			ignored:   true,
			handleRet: nvml.ERROR_INVALID_ARGUMENT,
			want:      true,
		},
		"GPU lost is not classified as a non-fatal XID": {
			eventType: HealthEventGPULost,
		},
		"unmonitored is not classified as a non-fatal XID": {
			eventType: HealthEventUnmonitored,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			parent := &GpuInfo{UUID: "GPU-parent-1", pciBusID: pciBusID}
			affectedDevice := &AllocatableDevice{Gpu: parent}
			if tc.mig {
				affectedDevice = &AllocatableDevice{
					MigStatic: &MigDeviceInfo{
						ParentUUID: parent.UUID,
						parent:     parent,
					},
				}
			}

			device := newMockRecoveryActionDevice(
				t,
				tc.action,
				tc.queryRet,
				tc.fieldRet,
				nvml.VALUE_TYPE_UNSIGNED_INT,
			)

			nvmllib := &mockNVMLLibrary{
				deviceGetHandleByPciBusIdFunc: func(got string) (nvml.Device, nvml.Return) {
					assert.Equal(t, pciBusID, got)
					if tc.handleRet != nvml.SUCCESS {
						return nil, tc.handleRet
					}
					return device, nvml.SUCCESS
				},
			}

			skippedXids := make(map[uint64]bool)
			if tc.ignored {
				skippedXids[xid] = true
			}

			monitor := &nvmlDeviceHealthMonitor{
				nvmllib:     nvmllib,
				skippedXids: skippedXids,
			}
			event := &DeviceHealthEvent{
				EventType: tc.eventType,
				EventData: xid,
			}
			if tc.eventType == HealthEventXID {
				event.Devices = []*AllocatableDevice{affectedDevice}
			}

			assert.Equal(t, tc.want, monitor.IsEventNonFatal(event))

			wantHandleCalls := 0
			wantQueryCalls := 0
			if tc.eventType == HealthEventXID {
				wantHandleCalls = 1
				if tc.handleRet == nvml.SUCCESS {
					wantQueryCalls = 1
				}
			}
			assert.Equal(t, wantHandleCalls, nvmllib.deviceGetHandleByPciBusIdCalls)
			assert.Equal(t, wantQueryCalls, device.getFieldValuesCalls)
		})
	}
}

func TestAllocatableDevicesFindByAddress(t *testing.T) {
	parent := &GpuInfo{UUID: "GPU-parent-1", minor: 0, pciBusID: "0000:01:00.0"}
	fullGPU := &AllocatableDevice{Gpu: parent}
	staticMIG := &AllocatableDevice{
		MigStatic: &MigDeviceInfo{
			ParentUUID: parent.UUID,
			GIID:       2,
			CIID:       3,
			parent:     parent},
	}
	devices := AllocatableDevices{
		"gpu":    fullGPU,
		"static": staticMIG,
	}

	assert.Equal(t, fullGPU, devices.GetGPUDeviceByUUID(parent.UUID))
	assert.Nil(t, devices.GetGPUDeviceByUUID("GPU-unknown"))
	assert.Equal(t, staticMIG, devices.GetMigStaticDeviceByLiveTuple(&MigLiveTuple{
		ParentUUID: parent.UUID,
		GIID:       2,
		CIID:       3,
	}))
	assert.Nil(t, devices.GetMigStaticDeviceByLiveTuple(&MigLiveTuple{
		ParentUUID: parent.UUID,
		GIID:       2,
		CIID:       4,
	}))
	assert.Nil(t, devices.GetMigStaticDeviceByLiveTuple(&MigLiveTuple{
		ParentUUID: "GPU-unknown",
		GIID:       2,
		CIID:       3,
	}))
	assert.Nil(t, devices.GetMigStaticDeviceByLiveTuple(nil))
}

func TestAllocatableDevicesFindDynamicMIGBySpec(t *testing.T) {
	parent := &GpuInfo{UUID: "GPU-parent-1", minor: 0, pciBusID: "0000:01:00.0"}
	dynamicMIG := &AllocatableDevice{MigDynamic: &MigSpec{
		Parent:        parent,
		GIProfileInfo: nvml.GpuInstanceProfileInfo{Id: 19},
		Placement:     nvml.GpuInstancePlacement{Start: 0},
	}}
	devices := AllocatableDevices{"dynamic": dynamicMIG}

	tests := []struct {
		name string
		spec *MigSpecTuple
		want *AllocatableDevice
	}{
		{name: "exact match", spec: &MigSpecTuple{ParentPCIBusID: parent.pciBusID, ProfileID: 19, PlacementStart: 0}, want: dynamicMIG},
		{name: "profile mismatch", spec: &MigSpecTuple{ParentPCIBusID: parent.pciBusID, ProfileID: 14, PlacementStart: 0}},
		{name: "placement mismatch", spec: &MigSpecTuple{ParentPCIBusID: parent.pciBusID, ProfileID: 19, PlacementStart: 1}},
		{name: "parent mismatch", spec: &MigSpecTuple{ParentPCIBusID: "0000:02:00.0", ProfileID: 19, PlacementStart: 0}},
		{name: "nil spec"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, devices.GetMigDynamicDeviceByTuple(tc.spec))
		})
	}
}

func TestResolveDeviceByEventAddressUsesGPUUUIDIndex(t *testing.T) {
	parent := &GpuInfo{UUID: "GPU-parent-1", minor: 0, pciBusID: "0000:01:00.0"}
	staticMIG := &AllocatableDevice{
		MigStatic: &MigDeviceInfo{
			ParentUUID: parent.UUID,
			GIID:       2,
			CIID:       3,
			parent:     parent},
	}
	monitor := &nvmlDeviceHealthMonitor{
		perGPUAllocatable: &PerGPUAllocatableDevices{
			allocatablesMap: map[PCIBusID]AllocatableDevices{
				parent.pciBusID: {
					"static": staticMIG,
				},
			},
		},
		gpuInfosByUUID: map[string]*GpuInfo{parent.UUID: parent},
	}

	got, err := monitor.resolveDeviceByEventAddress(parent.UUID, nil, 2, 3)
	require.NoError(t, err)
	require.Equal(t, staticMIG, got)

	got, err = monitor.resolveDeviceByEventAddress(parent.UUID, nil, FullGPUInstanceID, 3)
	require.Nil(t, got)
	require.NoError(t, err)

	_, err = monitor.resolveDeviceByEventAddress("GPU-unknown", nil, 2, 3)
	require.ErrorContains(t, err, "failed to find parent GPU UUID")

}

func TestAllocatableDevicesFindRejectsWrongParent(t *testing.T) {
	parent := &GpuInfo{
		UUID:     "GPU-parent-1",
		pciBusID: "0000:01:00.0",
	}
	otherParent := &GpuInfo{
		UUID:     "GPU-parent-2",
		pciBusID: parent.pciBusID,
	}

	devices := AllocatableDevices{
		"wrong-gpu": {
			Gpu: otherParent,
		},
		"wrong-static-mig": {
			MigStatic: &MigDeviceInfo{
				ParentUUID: otherParent.UUID,
				GIID:       2,
				CIID:       3,
				parent:     otherParent,
			},
		},
	}
	require.Nil(t, devices.GetGPUDeviceByUUID(parent.UUID))
	require.Nil(t, devices.GetMigStaticDeviceByLiveTuple(&MigLiveTuple{
		ParentUUID: parent.UUID,
		GIID:       2,
		CIID:       3,
	}))
}

func TestHealthMonitorStartRequiresRegisteredEvents(t *testing.T) {
	m := &nvmlDeviceHealthMonitor{}
	require.ErrorContains(t, m.Start(context.Background()), "events have not been registered")
}

func TestGetDeviceIncludesHealthTaints(t *testing.T) {
	dev := &AllocatableDevice{Gpu: &GpuInfo{
		UUID:                  "GPU-1",
		minor:                 0,
		cudaComputeCapability: "9.0",
		driverVersion:         "580.0",
		cudaDriverVersion:     "13.0",
	}}
	taint := &resourceapi.DeviceTaint{
		Key:    TaintKeyXID,
		Value:  "43",
		Effect: resourceapi.DeviceTaintEffectNone,
	}
	require.True(t, dev.AddOrUpdateTaint(taint))

	got := dev.GetDevice(nil)

	require.Len(t, got.Taints, 1)
	assert.Equal(t, *taint, got.Taints[0])
}
func TestClearDynamicMIGXIDTaint(t *testing.T) {
	dynamic := &AllocatableDevice{MigDynamic: &MigSpec{}}
	dynamic.AddOrUpdateTaint(&resourceapi.DeviceTaint{
		Key:   TaintKeyXID,
		Value: "43",
	})
	dynamic.AddOrUpdateTaint(&resourceapi.DeviceTaint{
		Key: TaintKeyGPULost,
	})

	static := &AllocatableDevice{MigStatic: &MigDeviceInfo{}}
	static.AddOrUpdateTaint(&resourceapi.DeviceTaint{
		Key:   TaintKeyXID,
		Value: "43",
	})

	state := &DeviceState{
		perGPUAllocatable: &PerGPUAllocatableDevices{
			allocatablesMap: map[PCIBusID]AllocatableDevices{
				"0000:01:00.0": {
					"dynamic": dynamic,
					"static":  static,
				},
			},
		},
	}

	state.clearDynamicMIGXIDTaint("dynamic")
	state.clearDynamicMIGXIDTaint("static")

	require.Len(t, dynamic.Taints(), 1)
	assert.Equal(t, TaintKeyGPULost, dynamic.Taints()[0].Key)

	require.Len(t, static.Taints(), 1)
	assert.Equal(t, TaintKeyXID, static.Taints()[0].Key)
}
