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
	"maps"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/Masterminds/semver/v3"
	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/runtime"
	coreclientset "k8s.io/client-go/kubernetes"
	drametadatav1alpha1 "k8s.io/dynamic-resource-allocation/api/metadata/v1alpha1"
	drametadatav1beta1 "k8s.io/dynamic-resource-allocation/api/metadata/v1beta1"
	"k8s.io/dynamic-resource-allocation/kubeletplugin"
	"k8s.io/dynamic-resource-allocation/resourceslice"
	"k8s.io/klog/v2"

	"sigs.k8s.io/dra-driver-nvidia-gpu/pkg/featuregates"
	"sigs.k8s.io/dra-driver-nvidia-gpu/pkg/flock"
	drametrics "sigs.k8s.io/dra-driver-nvidia-gpu/pkg/metrics"
)

// DriverPrepUprepFlockPath is the path to a lock file used to make sure
// that calls to nodePrepareResource() / nodeUnprepareResource() never
// interleave, node-globally.
const DriverPrepUprepFlockFileName = "pu.lock"

type deviceHealthMonitor interface {
	Start(context.Context) error
	Stop()
	Unhealthy() <-chan *DeviceHealthEvent
	// Allows the driver to query the HealthMonitor's health policy
	IsEventNonFatal(event *DeviceHealthEvent) bool
}

type driver struct {
	client              coreclientset.Interface
	pluginhelper        *kubeletplugin.Helper
	state               *DeviceState
	pulock              *flock.Flock
	healthcheck         *healthcheck
	deviceHealthMonitor deviceHealthMonitor
	wg                  sync.WaitGroup
	// Idicates whether to use separate ResourceSlices for SharedCounters and
	// Devices (required for k8s 1.35+) or combined SharedCounters and Devices
	// in the same slice (required for k8s 1.34).
	useSplitResourceSlices bool
}

func NewDriver(ctx context.Context, config *Config) (*driver, error) {
	state, err := NewDeviceState(ctx, config)
	if err != nil {
		return nil, err
	}

	useSplitSlices := false

	if featuregates.Enabled(featuregates.DynamicMIG) {
		if !state.IsMigCapable() {
			klog.Warningf("DynamicMIG enabled but no MIG capable GPU found on this node; falling back to legacy Full GPU support")
		} else {
			// Could be done in NewDeviceState, but I want to make sure that the
			// checkpoint machinery is ready to use -- that's more obvious here.
			//
			// Generally, when `featuregates.DynamicMIG` is enabled, we have to make
			// difficult but good decisions about incarnated MIG devices found
			// during program startup. We could
			//
			// 1) assume they are under control of an external entity, and not
			// announce them. That's likely not true. As hard as we try, as part of
			// dynamic MIG device management, given enough time and circumstances,
			// we might actually leave a MIG device behind where we shouldn't (as of
			// bugs, as of aggressive operations / admin intervention, ...).
			//
			// 2) not do anythin special: not good; we would still announce the
			// corresponding abstract MIG device and once the scheduler assigns a
			// job, a relevant NodePrepareResources() call will try to create that
			// specific MIG device. And that will fail, because that MIG device
			// already exists -- users see something like "prepare devices failed:
			// error creating MIG device: error creating GPU instance for
			// 'gpu-0-mig-1g24gb-0': Insufficient Resources.
			//
			// 3) Use the node-local checkpoint as the source of truth. Any MIG
			// device that corresponds to "partially prepared" claims should be
			// destroyed, and any MIG device that is not mentioned in the checkpoint
			// at all must be destroyed). Both is done below. Only those of
			// completely prepared claims can stay; assuming that the central
			// scheduler state is equivalent. TODO: review if this logic is correct;
			// or if it potentially is too invasive for certain edge cases.
			state.DestroyUnknownMIGDevices(ctx)

			// Read Kubernetes API server version to determine which ResourceSlice
			// model to use.
			var err error
			useSplitSlices, err = shouldUseSplitResourceSlices(config.clientsets.Core)
			if err != nil {
				return nil, fmt.Errorf("failed to determine ResourceSlice model: %w", err)
			}
		}
	}

	puLockPath := filepath.Join(config.DriverPluginPath(), DriverPrepUprepFlockFileName)

	driver := &driver{
		client:                 config.clientsets.Core,
		state:                  state,
		pulock:                 flock.NewFlock(puLockPath),
		useSplitResourceSlices: useSplitSlices,
	}

	// Register NVML events before kubeletplugin.Start exposes Prepare/Unprepare.
	// On plugin restart, previously prepared devices and their workloads can remain
	// live and emit an XID before the kubelet service is available. NVML does not
	// retain events that occur before registration.
	if featuregates.Enabled(featuregates.NVMLDeviceHealthCheck) {
		deviceHealthMonitor, err := newNvmlDeviceHealthMonitor(config, state.perGPUAllocatable, state.nvdevlib)
		if err != nil {
			return nil, fmt.Errorf("failed to create NVML device health monitor: %w", err)
		}
		driver.deviceHealthMonitor = deviceHealthMonitor

		// Events recorded after registration remain queued until Start begins
		// waiting after the kubelet helper is available.
		if err := deviceHealthMonitor.RegisterEvents(); err != nil {
			return nil, fmt.Errorf("failed to register NVML device events: %w", err)
		}
	}

	opts := []kubeletplugin.Option{
		kubeletplugin.KubeClient(driver.client),
		kubeletplugin.NodeName(config.flags.nodeName),
		kubeletplugin.DriverName(DriverName),
		kubeletplugin.Serialize(false),
		kubeletplugin.RegistrarDirectoryPath(config.flags.kubeletRegistrarDirectoryPath),
		kubeletplugin.PluginDataDirectoryPath(config.DriverPluginPath()),
	}
	// KEP-5304: Enable Device Metadata support for the kubelet plugin implementation.
	// See: https://github.com/kubernetes/enhancements/tree/master/keps/sig-node/5304-dra-attributes-downward-api
	if featuregates.Enabled(featuregates.DeviceMetadata) {
		opts = append(opts, kubeletplugin.EnableDeviceMetadata(true, []schema.GroupVersion{
			drametadatav1beta1.SchemeGroupVersion,
			drametadatav1alpha1.SchemeGroupVersion,
		}))
	}
	// This plugin does not report device health (KEP-4680), so don't
	// advertise the DRAResourceHealth service to the kubelet.
	opts = append(opts, kubeletplugin.HealthService(false))
	helper, err := kubeletplugin.Start(ctx, driver, opts...)
	if err != nil {
		return nil, err
	}
	driver.pluginhelper = helper

	healthcheck, err := startHealthcheck(ctx, config, helper)
	if err != nil {
		return nil, fmt.Errorf("start healthcheck: %w", err)
	}
	driver.healthcheck = healthcheck

	// Pass `nodeUnprepareResource` function to the cleanup manager.
	if err := state.checkpointCleanupManager.Start(ctx, driver.nodeUnprepareResource); err != nil {
		return nil, fmt.Errorf("error starting CheckpointCleanupManager: %w", err)
	}

	if err := driver.publishResources(ctx, config); err != nil {
		return nil, err
	}

	if featuregates.Enabled(featuregates.NVMLDeviceHealthCheck) {
		// RegisterEvents can queue unmonitored events before this consumer
		// starts. Publish the initial ResourceSlices first, so subsequent health
		// updates republish tainted resources without an initial snapshot
		// overwriting them.
		// TODO: NVML does not replay XIDs emitted before registration. Because
		// health taints are not persisted, a restart can advertise a device as
		// healthy unless the fault emits another event. Persist health state or
		// validate recovery before clearing taints during startup.
		driver.wg.Add(1)
		go func() {
			defer driver.wg.Done()
			driver.deviceHealthEvents(ctx)
		}()
		if err := driver.deviceHealthMonitor.Start(ctx); err != nil {
			return nil, fmt.Errorf("failed to start device health monitor: %w", err)
		}
	}

	klog.V(4).Infof("Current kubelet plugin registration status: %s", helper.RegistrationStatus())

	return driver, nil
}

// GenerateDriverResources() returns the set of DRA ResourceSlices announced by
// this DRA driver to the system, using the Partitionable Devices paradigm.
func (d *driver) GenerateDriverResources(nodeName string) resourceslice.DriverResources {
	if d.useSplitResourceSlices {
		return d.generateSplitResourceSlices(nodeName)
	}
	return d.generateCombinedResourceSlices(nodeName)
}

// generateSplitResourceSlices generates ResourceSlices for DynamicMIG for k8s 1.35+.
// Creates G+1 resource slices for G physical GPUs:
// - One slice with all SharedCounters (one counter set per GPU).
// - For each GPU, one slice with devices only (full GPU + MIG partitions).
func (d *driver) generateSplitResourceSlices(nodeName string) resourceslice.DriverResources {
	var gpuslices []resourceslice.Slice
	var allCounterSets []resourceapi.CounterSet

	// Iterate through `perGPUAllocatable` map in predictable order
	for _, pciBusID := range slices.Sorted(maps.Keys(d.state.perGPUAllocatable.allocatablesMap)) {
		allocatable := d.state.perGPUAllocatable.allocatablesMap[pciBusID]
		var deviceSlice resourceslice.Slice
		var gpuInfo *GpuInfo

		// Stable sort order by devicename
		for _, devname := range slices.Sorted(maps.Keys(allocatable)) {
			device := allocatable[devname]
			klog.V(4).Infof("About to announce device %s", devname)

			// Remember this GPU so we can emit exactly one shared counter
			// set for it below. MIG partitions reference the parent GPU's
			// counter set by name, so the counter set must be emitted even
			// when the full GPU itself is not announced (e.g. on Ampere
			// with MIG mode enabled, where MIG cannot be toggled without a
			// GPU reset and so only MIG partitions are allocatable).
			if gpuInfo == nil {
				switch {
				case device.Gpu != nil:
					gpuInfo = device.Gpu
				case device.MigDynamic != nil:
					gpuInfo = device.MigDynamic.Parent
				}
			}

			// Add device/partition to the device-only slice for this GPU.
			deviceSlice.Devices = append(deviceSlice.Devices, device.PartGetDevice(d.state.config))
		}
		if gpuInfo != nil {
			allCounterSets = append(allCounterSets, gpuInfo.PartSharedCounterSets()...)
		}
		gpuslices = append(gpuslices, deviceSlice)
	}

	sharedCountersSlice := resourceslice.Slice{
		SharedCounters: allCounterSets,
	}

	// Emit the `sharedCountersSlice` first.
	gpuslices = append([]resourceslice.Slice{sharedCountersSlice}, gpuslices...)

	return resourceslice.DriverResources{
		Pools: map[string]resourceslice.Pool{
			nodeName: {Slices: gpuslices},
		},
	}
}

// generateCombinedResourceSlices generates ResourceSlices for DynamicMIG for k8s 1.34.
// Creates G resource slices for G physical GPUs, each containing both,
// SharedCounters and Devices.
func (d *driver) generateCombinedResourceSlices(nodeName string) resourceslice.DriverResources {
	var gpuslices []resourceslice.Slice

	// Iterate through `perGPUAllocatable` map in predictable order so that the
	// slices get published in predictable order.
	for _, pciBusID := range slices.Sorted(maps.Keys(d.state.perGPUAllocatable.allocatablesMap)) {
		allocatable := d.state.perGPUAllocatable.allocatablesMap[pciBusID]
		var slice resourceslice.Slice
		var gpuInfo *GpuInfo

		// Stable sort order by devicename -- makes the order of devices
		// presented in a resource slice reproducible. Good for debuggability /
		// readability, and leads to a minimal slice diff during kubelet plugin
		// restart (the slice diff is logged).
		for _, devname := range slices.Sorted(maps.Keys(allocatable)) {
			device := allocatable[devname]
			klog.V(4).Infof("About to announce device %s", devname)

			// Remember this GPU so we can emit exactly one shared counter
			// set for it below. MIG partitions reference the parent GPU's
			// counter set by name, so the counter set must be emitted even
			// when the full GPU itself is not announced (e.g. on Ampere
			// with MIG mode enabled, where MIG cannot be toggled without a
			// GPU reset and so only MIG partitions are allocatable).
			if gpuInfo == nil {
				switch {
				case device.Gpu != nil:
					gpuInfo = device.Gpu
				case device.MigDynamic != nil:
					gpuInfo = device.MigDynamic.Parent
				}
			}

			// Add all allocatable devices for this physical GPU to this slice.
			// This includes not-yet-manifested MIG devices, and the physical
			// GPU itself.
			slice.Devices = append(slice.Devices, device.PartGetDevice(d.state.config))
		}

		if gpuInfo != nil {
			slice.SharedCounters = gpuInfo.PartSharedCounterSets()
		}
		gpuslices = append(gpuslices, slice)
	}

	return resourceslice.DriverResources{
		Pools: map[string]resourceslice.Pool{
			nodeName: {Slices: gpuslices},
		},
	}
}

func (d *driver) Shutdown() error {
	if d == nil {
		return nil
	}

	if d.healthcheck != nil {
		d.healthcheck.Stop()
	}

	// Shut down long-lived NVML session.
	if featuregates.Enabled(featuregates.DynamicMIG) {
		d.state.nvdevlib.alwaysShutdown()
	}

	// Tear down the Fabric Manager connection, if one was opened.
	if d.state.fmManager != nil {
		if err := d.state.fmManager.Close(); err != nil {
			klog.Warningf("error closing Fabric Manager connection: %v", err)
		}
	}

	if d.deviceHealthMonitor != nil {
		d.deviceHealthMonitor.Stop()
	}

	d.wg.Wait()

	if err := d.state.checkpointCleanupManager.Stop(); err != nil {
		return fmt.Errorf("error stopping CheckpointCleanupManager: %w", err)
	}

	d.pluginhelper.Stop()
	return nil
}

func (d *driver) PrepareResourceClaims(ctx context.Context, claims []*resourceapi.ResourceClaim) (map[types.UID]kubeletplugin.PrepareResult, error) {

	if len(claims) == 0 {
		// That's probably the health check, log that on higher verbosity level
		klog.V(7).Infof("PrepareResourceClaims called with %d claim(s)", len(claims))
	} else {
		// Log canonical string representation for each claim injected here --
		// we've noticed that this can greatly facilitate debugging.
		klog.V(6).Infof("Prepare called for: %v", ClaimsToStrings(claims))
	}

	results := make(map[types.UID]kubeletplugin.PrepareResult)

	for _, claim := range claims {
		results[claim.UID] = d.nodePrepareResource(ctx, claim)
	}

	return results, nil
}

func (d *driver) UnprepareResourceClaims(ctx context.Context, claimRefs []kubeletplugin.NamespacedObject) (map[types.UID]error, error) {
	klog.V(6).Infof("Unprepare called for: %v", ClaimRefsToStrings(claimRefs))
	results := make(map[types.UID]error)
	for _, claimRef := range claimRefs {
		results[claimRef.UID] = d.nodeUnprepareResource(ctx, claimRef)
	}

	return results, nil
}

func (d *driver) HandleError(ctx context.Context, err error, msg string) {
	// For now we just follow the advice documented in the DRAPlugin API docs.
	// See: https://pkg.go.dev/k8s.io/apimachinery/pkg/util/runtime#HandleErrorWithContext
	runtime.HandleErrorWithContext(ctx, err, msg)
}

func (d *driver) WatchHealthStatus(context.Context, chan<- kubeletplugin.DeviceHealthReport) error {
	return kubeletplugin.ErrHealthNotSupported
}

func (d *driver) nodePrepareResource(ctx context.Context, claim *resourceapi.ResourceClaim) kubeletplugin.PrepareResult {
	t0 := time.Now()
	// Instead of a global prepare/unprepare (PU) lock, we could rely on
	// fine-grained checkpoint locking, which was proven to work correctly in
	// case of DynamicMIG mode. However, out of caution, retain this global PU
	// lock for now in all modes (re-evaluate the performance impact at a later
	// time).

	release, err := d.pulock.Acquire(ctx, flock.WithTimeout(10*time.Second))
	if err != nil {
		drametrics.IncNodePrepareError(DriverName, "lock_acquire")
		return kubeletplugin.PrepareResult{
			Err: fmt.Errorf("error acquiring prep/unprep lock: %w", err),
		}
	}
	defer release()
	doneInFlight := drametrics.TrackInFlight(DriverName, "prepare")
	defer doneInFlight()
	klog.V(6).Infof("t_prep_lock_acq %.3f s", time.Since(t0).Seconds())

	cs := ResourceClaimToString(claim)
	tprep0 := time.Now()
	devs, err := d.state.Prepare(ctx, claim)
	klog.V(6).Infof("t_prep %.3f s (claim %s)", time.Since(tprep0).Seconds(), cs)

	if err != nil {
		drametrics.IncNodePrepareError(DriverName, "prepare_devices")
		return kubeletplugin.PrepareResult{
			Err: fmt.Errorf("error preparing devices for claim %s: %w", cs, err),
		}
	}

	if featuregates.Enabled(featuregates.PassthroughSupport) {
		// Re-advertise updated resourceslice after preparing devices.
		if err = d.publishResources(ctx, d.state.config); err != nil {
			drametrics.IncNodePrepareError(DriverName, "publish_resources")
			return kubeletplugin.PrepareResult{
				Err: fmt.Errorf("failed to publish resources after preparing claim %s: %w", cs, err),
			}
		}
	}

	klog.V(1).Infof("Returning newly prepared devices for claim '%s': %v", cs, devs)
	drametrics.ObserveRequest(DriverName, "prepare", time.Since(t0))
	return kubeletplugin.PrepareResult{Devices: devs}
}

func (d *driver) nodeUnprepareResource(ctx context.Context, claimRef kubeletplugin.NamespacedObject) error {
	t0 := time.Now()

	release, err := d.pulock.Acquire(ctx, flock.WithTimeout(10*time.Second))
	if err != nil {
		drametrics.IncNodeUnprepareError(DriverName, "lock_acquire")
		return fmt.Errorf("error acquiring prep/unprep lock: %w", err)
	}
	defer release()
	doneInFlight := drametrics.TrackInFlight(DriverName, "unprepare")
	defer doneInFlight()
	klog.V(6).Infof("t_unprep_lock_acq %.3f s", time.Since(t0).Seconds())

	cs := claimRef.String()
	tunprep0 := time.Now()
	taintRemovedRepublish, err := d.state.Unprepare(ctx, claimRef)
	klog.V(6).Infof("t_unprep %.3f s (claim %s)", time.Since(tunprep0).Seconds(), cs)

	if err != nil {
		drametrics.IncNodeUnprepareError(DriverName, "unprepare_devices")
		return fmt.Errorf("error unpreparing devices for claim %v: %w", claimRef.String(), err)
	}

	if featuregates.Enabled(featuregates.PassthroughSupport) ||
		(featuregates.Enabled(featuregates.DynamicMIG) &&
			featuregates.Enabled(featuregates.NVMLDeviceHealthCheck) &&
			taintRemovedRepublish) {
		// Re-advertise updated resourceslice after unpreparing devices
		// or removed a Dynamic MIG XID taint.
		if err = d.publishResources(ctx, d.state.config); err != nil {
			drametrics.IncNodeUnprepareError(DriverName, "publish_resources")
			return fmt.Errorf("error publishing resources: %w", err)
		}
	}

	drametrics.ObserveRequest(DriverName, "unprepare", time.Since(t0))
	return nil
}

func (d *driver) publishResources(ctx context.Context, config *Config) error {

	if featuregates.Enabled(featuregates.DynamicMIG) {
		// From KEP 4815: "we will add client-side validation in the
		// ResourceSlice controller helper, so that any errors in the
		// ResourceSlices will be caught before they even are applied to the
		// APIServer" -- the helper below is being referred to.
		//
		// TODO: implement error handler for bad slices:
		// https://github.com/kubernetes/kubernetes/commit/a171795e313ee9f407fef4897c1a1e2052120991
		klog.V(4).Infof("featuregates.DynamicMIG enabled: construct ResourceSlice objects according to KEP 4815 (partitionable devices)")
		resources := d.GenerateDriverResources(config.flags.nodeName)
		if err := d.pluginhelper.PublishResources(ctx, resources); err != nil {
			return err
		}
		return nil
	}

	// Enumerate the set of GPU, MIG and VFIO devices and publish them
	var resourceSlice resourceslice.Slice
	for _, devices := range d.state.perGPUAllocatable.allocatablesMap {
		for _, device := range devices {
			klog.V(4).Infof("About to announce device %s", device.GetDevice(config).Name)
			resourceSlice.Devices = append(resourceSlice.Devices, device.GetDevice(config))
		}
	}

	resources := resourceslice.DriverResources{
		Pools: map[string]resourceslice.Pool{
			config.flags.nodeName: {Slices: []resourceslice.Slice{resourceSlice}},
		},
	}

	if err := d.pluginhelper.PublishResources(ctx, resources); err != nil {
		return err
	}

	return nil

}

func (d *driver) deviceHealthEvents(ctx context.Context) {
	klog.V(4).Info("Starting to watch for device health notifications")
	for {
		select {
		case <-ctx.Done():
			klog.V(6).Info("Stop processing device health notifications")
			return
		case event, ok := <-d.deviceHealthMonitor.Unhealthy():
			if !ok {
				// NVML based deviceHealthMonitor is expected to close only during driver Shutdown.
				klog.V(6).Info("Health monitor channel closed")
				return
			}

			taint := healthEventToTaint(d.deviceHealthMonitor, event)
			modified := false
			for _, dev := range event.Devices {
				klog.Warningf("Received %s health event for device %s", event.EventType, dev.CanonicalName())
				if d.state.AddDeviceTaint(dev, taint) {
					modified = true
				}
			}
			if !modified {
				continue
			}

			// NOTE: We only log an error on publish failure and do not retry.
			// If this publish fails, our in-memory health update succeeds but the
			// ResourceSlice in the API server remains stale and still advertises the
			// now-unhealthy device as allocatable. Until a later publish succeeds,
			// the scheduler and other consumers will continue to see the unhealthy
			// device as available, and new pods may be placed onto hardware we know
			// is unusable. If publishes continue to fail (e.g., API server issues),
			// the cluster can remain in this inconsistent state indefinitely.
			// This is a temporary compromise while device taints/tolerations (KEP-5055)
			// are available as a Beta feature. An interim improvement could be adding
			// a retry/backoff or switch to patch updates instead of full republish.
			klog.V(4).Infof("Republishing ResourceSlice: %d device(s) tainted with %s=%q (effect=%s)",
				len(event.Devices), taint.Key, taint.Value, taint.Effect)

			// NOTE: GPU_LOST and unmonitored events are already batched at the
			// sender (all affected devices arrive in a single DeviceHealthEvent).
			// XID events are still per-device and may cause repeated publishes.
			// TODO: Add receiver-side event aggregation before PublishResources.
			// Evaluate two strategies:
			// 1. Channel drain: non-blocking pull of all pending events (Pro: zero latency; Con: susceptible to NVML lag).
			// 2. Timer debounce: e.g., 50ms window (Pro: standard K8s API protection; Con: slight delay).
			// This also needs to be handled properly in the recovery path.
			if err := d.publishResources(ctx, d.state.config); err != nil {
				klog.Errorf("Failed to publish resources after taint update: %v", err)
			}
		}
	}
}

// shouldUseSplitResourceSlices detects the Kubernetes server version and
// returns true if separate ResourceSlices should be used for SharedCounters and
// Devices (required for k8s 1.35+), or false if they must be combined (k8s
// 1.34).
func shouldUseSplitResourceSlices(client coreclientset.Interface) (bool, error) {
	v, err := getAPIServerVersion(client)
	if err != nil {
		return false, fmt.Errorf("API server version detection failed: %w", err)
	}

	if v.LessThan(semver.MustParse("1.35.0")) {
		klog.V(2).Infof("Detected Kubernetes version %s (< 1.35), plan to use combined ResourceSlices with SharedCounters and Devices", v)
		return false, nil
	}

	klog.V(2).Infof("Detected Kubernetes version %s (>= 1.35), plan to use separate ResourceSlices for SharedCounters and Devices", v)
	return true, nil
}

func getAPIServerVersion(client coreclientset.Interface) (*semver.Version, error) {
	discoveryClient := client.Discovery()
	v, err := discoveryClient.ServerVersion()
	if err != nil {
		return nil, fmt.Errorf("failed to get server version: %w", err)
	}

	// `v.GitVersion`` is e.g. "v1.35.2"; semver.NewVersion handes the v prefix.
	semver, err := semver.NewVersion(v.GitVersion)
	if err != nil {
		return nil, fmt.Errorf("failed to parse version '%s': %w", v.GitVersion, err)
	}

	return semver, nil
}

// TODO: implement loop to remove CDI files from the CDI path for claimUIDs
//       that have been removed from the AllocatedClaims map.
// func (d *driver) cleanupCDIFiles(wg *sync.WaitGroup) chan error {
// 	errors := make(chan error)
// 	return errors
// }
//
// TODO: implement loop to remove mpsControlDaemon folders from the mps
//       path for claimUIDs that have been removed from the AllocatedClaims map.
// func (d *driver) cleanupMpsControlDaemonArtifacts(wg *sync.WaitGroup) chan error {
// 	errors := make(chan error)
// 	return errors
// }
