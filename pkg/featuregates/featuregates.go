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

package featuregates

import (
	"fmt"
	"sync"

	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/version"
	"k8s.io/component-base/featuregate"
	logsapi "k8s.io/component-base/logs/api/v1"
)

// featureGateEmulationVersion is the version passed to component-base's versioned
// feature gate. It must use Kubernetes-style versions (major.minor matching the
// Kubernetes release line), not the driver SemVer from VERSION.
//
// k8s.io/component-base/logs registers gates such as ContextualLogging at v1.24+
// / v1.30+. If emulation were driver v0.4, those specs compare as "newer than"
// emulation and the gate falls through to PreAlpha, which makes SetFromMap panic.
//
// Driver-local gates below use 0.x Version fields, they remain visible because
// 0.x < 1.y under apimachinery version ordering.
//
// Keep this major.minor aligned with the Kubernetes release we vendor in go.mod
// (k8s.io/* modules) and bump it whenever those dependencies move to a new kube minor.
//
// TODO: optionally isolate driver-only gates in their own registry so emulation can
// stay purely on driver SemVer without sharing a single version stream with component-base.
var featureGateEmulationVersion = version.MajorMinor(1, 37)

const (
	// TimeSlicingSettings allows timeslicing settings to be customized.
	TimeSlicingSettings featuregate.Feature = "TimeSlicingSettings"

	// MPSSupport allows MPS (Multi-Process Service) settings to be specified.
	MPSSupport featuregate.Feature = "MPSSupport"

	// IMEXDaemonsWithDNSNames allows using DNS names instead of raw IPs for IMEX daemons.
	IMEXDaemonsWithDNSNames featuregate.Feature = "IMEXDaemonsWithDNSNames"

	// PassthroughSupport allows gpus to be configured with the vfio-pci driver.
	PassthroughSupport featuregate.Feature = "PassthroughSupport"

	// NVMLDeviceHealthCheck allows Device Health Checking using NVML.
	NVMLDeviceHealthCheck featuregate.Feature = "NVMLDeviceHealthCheck"

	// Enable dynamic MIG device management.
	DynamicMIG featuregate.Feature = "DynamicMIG"

	// ComputeDomainCliques enables using ComputeDomainClique CRD objects instead of
	// storing daemon info directly in ComputeDomainStatus.Nodes.
	ComputeDomainCliques featuregate.Feature = "ComputeDomainCliques"

	// CrashOnNVLinkFabricErrors causes the kubelet plugin to crash instead of
	// falling back to non-fabric mode when NVLink fabric errors are detected.
	CrashOnNVLinkFabricErrors featuregate.Feature = "CrashOnNVLinkFabricErrors"

	// DeviceMetadata allows the kubelet plugin to generate device metadata files
	// in the workloads for prepared devices.
	DeviceMetadata featuregate.Feature = "DeviceMetadata"

	// FabricManagerPartitioning enables Fabric Manager (NVSwitch) partition
	// management for full-GPU (gpu.nvidia.com) devices and Passthrough VFIO
	// devices. When enabled, Prepare activates the FM partition whose member
	// set exactly matches the claim's allocated physical GPUs; a non-matching
	// set fails Prepare. ResourceClaims should constrain allocation with
	// matchAttribute on gpu.nvidia.com/partitionN. Requires Fabric Manager
	// running with FABRIC_MODE=1. Independent of PassthroughSupport: with both
	// gates on, full-GPU and VFIO claims are partitioned; with only this gate
	// on, only full-GPU claims are.
	FabricManagerPartitioning featuregate.Feature = "FabricManagerPartitioning"

	// HostManagedIMEXDaemon gates whether the imex deployment mode
	// may be set to "hostManaged" (see pkg/imex). It does not directly change
	// driver behavior, it only unlocks the mode for clusters where
	// the cluster admin owns the host nvidia-imex daemon lifecycle.
	HostManagedIMEXDaemon featuregate.Feature = "HostManagedIMEXDaemon"

	// DRAListTypeAttributes allows the GPU kubelet plugin to publish list-valued
	// DRA device attributes. The cluster must have the Kubernetes feature gate
	// of the same name enabled before enabling this in the driver.
	DRAListTypeAttributes featuregate.Feature = "DRAListTypeAttributes"

	// ConsumableShares enables publishing consumable capacity and multi-allocation
	// (AllowMultipleAllocations) support for devices in ResourceSlices, allowing
	// multiple ResourceClaims to share the same GPU or MIG device when configured
	// via --consumable-shares. Note: MPS sharing is not supported when consumable
	// shares is enabled.
	ConsumableShares featuregate.Feature = "ConsumableShares"

	// ResourceClaimDeviceStatus enables the kubelet plugin to publish
	// per-device status (KEP-4817) into ResourceClaim.status.devices upon
	// claim preparation: which physical GPU / MIG device a claim got (UUID,
	// product name, driver version, MIG profile/parent). This gives a durable
	// pod->device mapping that survives device removal from ResourceSlices.
	// The cluster must have the Kubernetes feature gate
	// DRAResourceClaimDeviceStatus enabled, and RBAC must allow the plugin to
	// update resourceclaims/status (see the Helm chart, which grants this
	// automatically when the gate is enabled via .Values.featureGates).
	ResourceClaimDeviceStatus featuregate.Feature = "ResourceClaimDeviceStatus"
)

// Feature gate Version fields use driver SemVer major.minor.
// Former calendar-based release lines map to SemVer minors for reference when
// adding new VersionedSpecs or backport notes:
//   ~25.3 -> 0.1, ~25.8 -> 0.2, ~25.12 -> 0.3, ~26.4 -> 0.4
//

// defaultFeatureGates contains the default settings for all project-specific feature gates.
// These will be registered with the standard Kubernetes feature gate system.
var defaultFeatureGates = map[featuregate.Feature]featuregate.VersionedSpecs{
	TimeSlicingSettings: {
		{
			Default:    false,
			PreRelease: featuregate.Alpha,
			Version:    version.MajorMinor(0, 2),
		},
	},
	MPSSupport: {
		{
			Default:    false,
			PreRelease: featuregate.Alpha,
			Version:    version.MajorMinor(0, 2),
		},
	},
	IMEXDaemonsWithDNSNames: {
		{
			Default:    true,
			PreRelease: featuregate.Beta,
			Version:    version.MajorMinor(0, 2),
		},
	},
	PassthroughSupport: {
		{
			Default:    false,
			PreRelease: featuregate.Alpha,
			Version:    version.MajorMinor(0, 3),
		},
	},
	DynamicMIG: {
		{
			Default:    false,
			PreRelease: featuregate.Alpha,
			Version:    version.MajorMinor(0, 3),
		},
	},
	NVMLDeviceHealthCheck: {
		{
			Default:    false,
			PreRelease: featuregate.Alpha,
			Version:    version.MajorMinor(0, 3),
		},
	},
	ComputeDomainCliques: {
		{
			Default:    true,
			PreRelease: featuregate.Beta,
			Version:    version.MajorMinor(0, 3),
		},
	},
	CrashOnNVLinkFabricErrors: {
		{
			Default:    true,
			PreRelease: featuregate.Beta,
			Version:    version.MajorMinor(0, 3),
		},
	},
	DeviceMetadata: {
		{
			Default:    false,
			PreRelease: featuregate.Alpha,
			Version:    version.MajorMinor(0, 4),
		},
	},
	FabricManagerPartitioning: {
		{
			Default:    false,
			PreRelease: featuregate.Alpha,
			Version:    version.MajorMinor(0, 5),
		},
	},
	HostManagedIMEXDaemon: {
		{
			Default:    false,
			PreRelease: featuregate.Alpha,
			Version:    version.MajorMinor(0, 5),
		},
	},
	DRAListTypeAttributes: {
		{
			Default:    false,
			PreRelease: featuregate.Alpha,
			Version:    version.MajorMinor(0, 4),
		},
	},
	ConsumableShares: {
		{
			Default:    false,
			PreRelease: featuregate.Alpha,
			Version:    version.MajorMinor(0, 5),
		},
	},
	ResourceClaimDeviceStatus: {
		{
			Default:    false,
			PreRelease: featuregate.Alpha,
			Version:    version.MajorMinor(0, 5),
		},
	},
}

var (
	featureGatesOnce sync.Once
	featureGates     featuregate.MutableVersionedFeatureGate
)

// FeatureGates instantiates and returns the package-level singleton representing
// the set of all feature gates and their values.
// It contains both project-specific feature gates and standard Kubernetes logging feature gates.
func FeatureGates() featuregate.MutableVersionedFeatureGate {
	if featureGates == nil {
		featureGatesOnce.Do(func() {
			featureGates = newFeatureGates(featureGateEmulationVersion)
		})
	}
	return featureGates
}

// newFeatureGates instantiates a new set of feature gates with both standard Kubernetes
// logging feature gates and project-specific feature gates, along with appropriate default values.
// Mostly used for testing.
func newFeatureGates(version *version.Version) featuregate.MutableVersionedFeatureGate {
	// Create a versioned feature gate with the specified version
	// This ensures proper version handling for our feature gates
	fg := featuregate.NewVersionedFeatureGate(version)

	// Add standard Kubernetes logging feature gates
	utilruntime.Must(logsapi.AddFeatureGates(fg))

	// Add project-specific feature gates
	utilruntime.Must(fg.AddVersioned(defaultFeatureGates))

	// Override default logging feature gate values
	loggingOverrides := map[string]bool{
		string(logsapi.ContextualLogging): true,
	}
	utilruntime.Must(fg.SetFromMap(loggingOverrides))

	return fg
}

// ValidateFeatureGates validates feature gate dependencies and returns an error if
// any dependencies are not satisfied.
func ValidateFeatureGates() error {
	// ComputeDomainCliques requires IMEXDaemonsWithDNSNames
	if Enabled(ComputeDomainCliques) && !Enabled(IMEXDaemonsWithDNSNames) {
		return fmt.Errorf("feature gate %s requires %s to also be enabled", ComputeDomainCliques, IMEXDaemonsWithDNSNames)
	}

	if Enabled(DynamicMIG) && Enabled(PassthroughSupport) {
		return fmt.Errorf("feature gate %s is currently mutually exclusive with %s", DynamicMIG, PassthroughSupport)
	}

	if Enabled(DynamicMIG) && Enabled(MPSSupport) {
		return fmt.Errorf("feature gate %s is currently mutually exclusive with %s", DynamicMIG, MPSSupport)
	}

	if Enabled(PassthroughSupport) && Enabled(NVMLDeviceHealthCheck) {
		return fmt.Errorf("feature gate %s is currently mutually exclusive with %s", PassthroughSupport, NVMLDeviceHealthCheck)
	}

	if Enabled(DeviceMetadata) && !Enabled(PassthroughSupport) {
		return fmt.Errorf("feature gate %s requires %s to also be enabled", DeviceMetadata, PassthroughSupport)
	}

	return nil
}

// Enabled returns true if the specified feature gate is enabled in the global FeatureGates singleton.
// This is a convenience function that uses the global feature gate registry.
func Enabled(feature featuregate.Feature) bool {
	return FeatureGates().Enabled(feature)
}

// KnownFeatures returns a list of known feature gates with their descriptions.
func KnownFeatures() []string {
	return FeatureGates().KnownFeatures()
}

// ToMap returns all known feature gates as a map[string]bool suitable for
// template rendering (e.g., {"FeatureA": true, "FeatureB": false}).
// Returns an empty map if no feature gates are configured.
func ToMap() map[string]bool {
	result := make(map[string]bool)
	for feature := range FeatureGates().GetAll() {
		result[string(feature)] = FeatureGates().Enabled(feature)
	}
	return result
}
