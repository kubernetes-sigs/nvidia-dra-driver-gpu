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
	"k8s.io/dynamic-resource-allocation/kubeletplugin"
)

type PreparedDeviceList []PreparedDevice
type PreparedDevices []*PreparedDeviceGroup

type PreparedDevice struct {
	Channel *PreparedComputeDomainChannel `json:"channel"`
	Daemon  *PreparedComputeDomainDaemon  `json:"daemon"`
}

type PreparedComputeDomainChannel struct {
	Info   *ComputeDomainChannelInfo `json:"info"`
	Device *CheckpointedDevice       `json:"device"`
}

type PreparedComputeDomainDaemon struct {
	Info   *ComputeDomainDaemonInfo `json:"info"`
	Device *CheckpointedDevice      `json:"device"`
}

type PreparedDeviceGroup struct {
	Devices     PreparedDeviceList `json:"devices"`
	ConfigState DeviceConfigState  `json:"configState"`
}

func (d PreparedDevice) Type() string {
	if d.Channel != nil {
		return ComputeDomainChannelType
	}
	if d.Daemon != nil {
		return ComputeDomainDaemonType
	}
	return UnknownDeviceType
}

func (d *PreparedDevice) CanonicalName() string {
	switch d.Type() {
	case ComputeDomainChannelType:
		return d.Channel.Info.CanonicalName()
	case ComputeDomainDaemonType:
		return d.Daemon.Info.CanonicalName()
	}
	panic("unexpected type for AllocatableDevice")
}

func (d *PreparedDevice) CanonicalIndex() string {
	switch d.Type() {
	case ComputeDomainChannelType:
		return d.Channel.Info.CanonicalIndex()
	case ComputeDomainDaemonType:
		return d.Daemon.Info.CanonicalIndex()
	}
	panic("unexpected type for AllocatableDevice")
}

func (l PreparedDeviceList) ComputeDomainChannels() PreparedDeviceList {
	var devices PreparedDeviceList
	for _, device := range l {
		if device.Type() == ComputeDomainChannelType {
			devices = append(devices, device)
		}
	}
	return devices
}

func (l PreparedDeviceList) ComputeDomainDaemons() PreparedDeviceList {
	var devices PreparedDeviceList
	for _, device := range l {
		if device.Type() == ComputeDomainDaemonType {
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

func (g *PreparedDeviceGroup) GetDevices() []kubeletplugin.Device {
	var devices []kubeletplugin.Device
	for _, device := range g.Devices {
		switch device.Type() {
		case ComputeDomainChannelType:
			devices = append(devices, kubeletplugin.Device(*device.Channel.Device))
		case ComputeDomainDaemonType:
			devices = append(devices, kubeletplugin.Device(*device.Daemon.Device))
		}
	}
	return devices
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
