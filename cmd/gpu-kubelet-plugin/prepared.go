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
	"slices"

	"k8s.io/dynamic-resource-allocation/kubeletplugin"
)

// Reflects a prepared MIG device, regardless of its origin (static MIG, or
// dynamic MIG). This string may(?) be exposed in the API (is it, though?).
// Before the dynamic MIG capability, we used "mig", so keep using it for now.
// May just be an implementation detail.
const PreparedMigDeviceType = "mig"

type PreparedDeviceList []PreparedDevice
type PreparedDevices []*PreparedDeviceGroup

type PreparedDevice struct {
	// Represents a prepared full GPU.
	Gpu *PreparedGpu `json:"gpu"`
	// Represents a prepared MIG device, regardless of whether this was created
	// via the 'dynamic MIG' flow or if it is a pre-created (static) MIG device.
	Mig  *PreparedMigDevice  `json:"mig"`
	Vfio *PreparedVfioDevice `json:"vfio,omitempty"`
}

type PreparedGpu struct {
	Info   *GpuInfo            `json:"info"`
	Device *CheckpointedDevice `json:"device"`
}

type PreparedMigDevice struct {
	// Specifc, created device. Detail needed for deletion and book-keeping.
	// Note that this is either created via the 'static MIG' flow or the
	// 'dynamic MIG' flow -- in any case, it represents a MIG device that
	// currently exists (incarnated, concrete).
	Concrete *MigLiveTuple       `json:"concrete"`
	Device   *CheckpointedDevice `json:"device"`
}

type PreparedVfioDevice struct {
	Info   *VfioDeviceInfo     `json:"info"`
	Device *CheckpointedDevice `json:"device"`
}

type PreparedDeviceGroup struct {
	Devices     PreparedDeviceList `json:"devices"`
	ConfigState DeviceConfigState  `json:"configState"`
}

func (d PreparedDevice) Type() string {
	if d.Gpu != nil {
		return GpuDeviceType
	}
	if d.Mig != nil {
		return PreparedMigDeviceType
	}
	if d.Vfio != nil {
		return VfioDeviceType
	}
	return UnknownDeviceType
}

func (d *PreparedDevice) CanonicalName() string {
	switch d.Type() {
	case GpuDeviceType:
		return d.Gpu.Device.DeviceName
	case PreparedMigDeviceType:
		return d.Mig.Device.DeviceName
	case VfioDeviceType:
		return d.Vfio.Device.DeviceName
	}
	panic("unexpected type for AllocatableDevice")
}

// Return only devices representing full, physical GPUs.
func (l PreparedDeviceList) Gpus() PreparedDeviceList {
	var devices PreparedDeviceList
	for _, device := range l {
		if device.Type() == GpuDeviceType {
			devices = append(devices, device)
		}
	}
	return devices
}

func (l PreparedDeviceList) MigDevices() PreparedDeviceList {
	var devices PreparedDeviceList
	for _, device := range l {
		if device.Type() == PreparedMigDeviceType {
			devices = append(devices, device)
		}
	}
	return devices
}

func (l PreparedDeviceList) VfioDevices() PreparedDeviceList {
	var devices PreparedDeviceList
	for _, device := range l {
		if device.Type() == VfioDeviceType {
			devices = append(devices, device)
		}
	}
	return devices
}

func (d PreparedDevices) GetDevices() []kubeletplugin.Device {
	var devices []kubeletplugin.Device
	for _, group := range d {
		devices = append(devices, group.GetDevices()...)
	}
	return devices
}

func (d PreparedDevices) GetDeviceNames() []DeviceName {
	var names []DeviceName
	for _, group := range d {
		names = append(names, group.GetDeviceNames()...)
	}
	return names
}

func (g *PreparedDeviceGroup) GetDevices() []kubeletplugin.Device {
	var devices []kubeletplugin.Device
	for _, device := range g.Devices {
		switch device.Type() {
		case GpuDeviceType:
			devices = append(devices, kubeletplugin.Device(*device.Gpu.Device))
		case PreparedMigDeviceType:
			devices = append(devices, kubeletplugin.Device(*device.Mig.Device))
		case VfioDeviceType:
			devices = append(devices, kubeletplugin.Device(*device.Vfio.Device))
		}
	}
	return devices
}

func (g *PreparedDeviceGroup) GetDeviceNames() []DeviceName {
	var names []DeviceName
	for _, device := range g.Devices {
		names = append(names, device.CanonicalName())
	}
	return names
}

// UUIDs for full GPUs, MIG devices, and Vfio devices.
func (l PreparedDeviceList) UUIDs() []string {
	uuids := append(l.GpuUUIDs(), l.MigDeviceUUIDs()...)
	uuids = append(uuids, l.VfioDeviceUUIDs()...)
	slices.Sort(uuids)
	return uuids
}

// UUIDs for full GPUs, MIG devices, and Vfio devices.
func (g *PreparedDeviceGroup) UUIDs() []string {
	uuids := append(g.GpuUUIDs(), g.MigDeviceUUIDs()...)
	uuids = append(uuids, g.VfioDeviceUUIDs()...)
	slices.Sort(uuids)
	return uuids
}

// UUIDs for full GPUs, MIG devices, and Vfio devices.
func (d PreparedDevices) UUIDs() []string {
	uuids := append(d.GpuUUIDs(), d.MigDeviceUUIDs()...)
	uuids = append(uuids, d.VfioDeviceUUIDs()...)
	slices.Sort(uuids)
	return uuids
}

// UUIDs only for full GPUs.
func (l PreparedDeviceList) GpuUUIDs() []string {
	var uuids []string
	for _, device := range l.Gpus() {
		uuids = append(uuids, device.Gpu.Info.UUID)
	}
	slices.Sort(uuids)
	return uuids
}

func (g *PreparedDeviceGroup) GpuUUIDs() []string {
	return g.Devices.Gpus().UUIDs()
}

func (d PreparedDevices) GpuUUIDs() []string {
	var uuids []string
	for _, group := range d {
		uuids = append(uuids, group.GpuUUIDs()...)
	}
	slices.Sort(uuids)
	return uuids
}

func (l PreparedDeviceList) MigDeviceUUIDs() []string {
	var uuids []string
	for _, device := range l.MigDevices() {
		uuids = append(uuids, device.Mig.Concrete.MigUUID)
	}
	slices.Sort(uuids)
	return uuids
}

func (g *PreparedDeviceGroup) MigDeviceUUIDs() []string {
	return g.Devices.MigDevices().UUIDs()
}

func (d PreparedDevices) MigDeviceUUIDs() []string {
	var uuids []string
	for _, group := range d {
		uuids = append(uuids, group.MigDeviceUUIDs()...)
	}
	slices.Sort(uuids)
	return uuids
}

func (g *PreparedDeviceGroup) VfioDeviceUUIDs() []string {
	return g.Devices.VfioDevices().UUIDs()
}

func (l PreparedDeviceList) VfioDeviceUUIDs() []string {
	var uuids []string
	for _, device := range l.VfioDevices() {
		uuids = append(uuids, device.Vfio.Info.UUID)
	}
	slices.Sort(uuids)
	return uuids
}

func (d PreparedDevices) VfioDeviceUUIDs() []string {
	var uuids []string
	for _, group := range d {
		uuids = append(uuids, group.VfioDeviceUUIDs()...)
	}
	slices.Sort(uuids)
	return uuids
}

// GetNonAdminDevices returns a map of device names that were requested
// without admin access in the prepared claim.
func (c *PreparedClaim) GetNonAdminDevices() map[string]struct{} {
	if c.Status.Allocation == nil {
		return map[string]struct{}{}
	}

	requested := make(map[string]struct{}, len(c.Status.Allocation.Devices.Results))

	for _, r := range c.Status.Allocation.Devices.Results {
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
