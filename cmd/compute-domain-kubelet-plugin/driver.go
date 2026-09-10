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
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"time"

	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/runtime"
	coreclientset "k8s.io/client-go/kubernetes"
	"k8s.io/dynamic-resource-allocation/kubeletplugin"
	"k8s.io/dynamic-resource-allocation/resourceslice"
	"k8s.io/klog/v2"

	"sigs.k8s.io/dra-driver-nvidia-gpu/pkg/flock"
	drametrics "sigs.k8s.io/dra-driver-nvidia-gpu/pkg/metrics"
	"sigs.k8s.io/dra-driver-nvidia-gpu/pkg/workqueue"
)

// computeDomainPublishedDevices returns the devices the kubelet plugin should
// advertise in its node ResourceSlice. IMEX channel devices other than
// channel 0 are never advertised here (they are advertised as a network
// resource from the control plane instead). ComputeDomain daemon devices are
// additionally omitted when hostManaged is true, since daemon claims are
// never valid in that mode (see applyComputeDomainDaemonConfig).
func computeDomainPublishedDevices(allocatable AllocatableDevices, hostManaged bool) []resourceapi.Device {
	var devices []resourceapi.Device
	for _, device := range allocatable {
		if device.Type() == ComputeDomainChannelType && device.Channel.ID != 0 {
			continue
		}
		if hostManaged && device.Type() == ComputeDomainDaemonType {
			continue
		}
		devices = append(devices, device.GetDevice())
	}
	return devices
}

const (
	// ErrorRetryMaxTimeout limits the amount of time spent in the request
	// handlers UnprepareResourceClaims() and PrepareResourceClaims(), so that
	// we send a response to the kubelet in a predictable amount of time. Within
	// that deadline, retryable errors are retried (with backoff) via the
	// workqueue abstraction.
	ErrorRetryMaxTimeout = 45 * time.Second
	// DriverPrepUprepFlockPath is the path to a lock file used to make sure
	// that calls to nodePrepareResource() / nodeUnprepareResource() never
	// interleave, node-globally.
	DriverPrepUprepFlockFileName = "pu.lock"
)

// permanentError defines an error indicating that it is permanent.
// By default, every error will be retried up to ErrorRetryMaxTimeout.
// Errors marked as permanent will not be retried.
type permanentError struct{ error }

func isPermanentError(err error) bool {
	return errors.As(err, &permanentError{})
}

type driver struct {
	client       coreclientset.Interface
	pluginhelper *kubeletplugin.Helper
	state        *DeviceState
	pulock       *flock.Flock
	healthcheck  *healthcheck
}

func NewDriver(ctx context.Context, config *Config) (*driver, error) {
	state, err := NewDeviceState(ctx, config)
	if err != nil {
		return nil, err
	}

	puLockPath := filepath.Join(config.DriverPluginPath(), DriverPrepUprepFlockFileName)

	driver := &driver{
		client: config.clientsets.Core,
		state:  state,
		pulock: flock.NewFlock(puLockPath),
	}

	helper, err := kubeletplugin.Start(
		ctx,
		driver,
		kubeletplugin.KubeClient(driver.client),
		kubeletplugin.NodeName(config.flags.nodeName),
		kubeletplugin.DriverName(DriverName),
		// By default, the DRA library serializes (un)prepare calls. That is, at
		// most one such call is exposed to the driver at any given time.
		// Disable that behavior: this driver has codependent prepare() actions
		// (where for the first prepare() to eventually complete, a second
		// prepare() must be incoming). Concurrency management for incoming
		// requests is done with this driver's work queue abstraction.
		kubeletplugin.Serialize(false),
		kubeletplugin.RegistrarDirectoryPath(config.flags.kubeletRegistrarDirectoryPath),
		kubeletplugin.PluginDataDirectoryPath(config.DriverPluginPath()),
		// This plugin does not report device health (KEP-4680), so don't
		// advertise the DRAResourceHealth service to the kubelet.
		kubeletplugin.HealthService(false),
	)
	if err != nil {
		return nil, err
	}
	driver.pluginhelper = helper

	// Enumerate the set of ComputeDomain devices to publish.
	var resourceSlice resourceslice.Slice
	resourceSlice.Devices = computeDomainPublishedDevices(state.allocatable, config.imexConfig.EffectiveHostManaged())

	resources := resourceslice.DriverResources{
		Pools: map[string]resourceslice.Pool{
			config.flags.nodeName: {Slices: []resourceslice.Slice{resourceSlice}},
		},
	}

	if err := state.computeDomainManager.Start(ctx); err != nil {
		return nil, fmt.Errorf("error starting ComputeDomain manager: %w", err)
	}

	// Pass checkpoint mutation functions to the cleanup manager.
	if err := state.checkpointCleanupManager.Start(ctx, driver.nodeUnprepareResource, driver.deleteExpiredPrepareAbortedClaimEntries); err != nil {
		return nil, fmt.Errorf("error starting CheckpointCleanupManager: %w", err)
	}

	healthcheck, err := setupHealthcheckPrimitives(ctx, config)
	if err != nil {
		return nil, fmt.Errorf("error setting up healthcheck primitives: %w", err)
	}
	driver.healthcheck = healthcheck

	if err := driver.pluginhelper.PublishResources(ctx, resources); err != nil {
		return nil, fmt.Errorf("error in PublishResources(): %w", err)
	}

	return driver, nil
}

func (d *driver) Shutdown() error {
	if d == nil {
		return nil
	}

	if err := d.state.computeDomainManager.Stop(); err != nil {
		return fmt.Errorf("error stopping ComputeDomainManager: %w", err)
	}

	if err := d.state.checkpointCleanupManager.Stop(); err != nil {
		return fmt.Errorf("error stopping CheckpointCleanupManager: %w", err)
	}

	if d.healthcheck != nil {
		d.healthcheck.Stop()
	}

	d.pluginhelper.Stop()
	return nil
}

// WatchHealthStatus implements [kubeletplugin.DRAPlugin]. The ComputeDomain
// plugin does not report device health (see also the HealthService option in
// NewDriver, which keeps the DRAResourceHealth service from being advertised).
func (d *driver) WatchHealthStatus(ctx context.Context, reports chan<- kubeletplugin.DeviceHealthReport) error {
	return kubeletplugin.ErrHealthNotSupported
}

func (d *driver) PrepareResourceClaims(ctx context.Context, claims []*resourceapi.ResourceClaim) (map[types.UID]kubeletplugin.PrepareResult, error) {
	klog.V(6).Infof("PrepareResourceClaims called with %d claim(s)", len(claims))

	var wg sync.WaitGroup
	ctx, cancel := context.WithTimeout(ctx, ErrorRetryMaxTimeout)
	workQueue := workqueue.New(workqueue.DefaultPrepUnprepRateLimiter())
	results := make(map[types.UID]kubeletplugin.PrepareResult)

	for _, claim := range claims {
		wg.Add(1)
		key := fmt.Sprintf("%v", claim.UID)
		workQueue.EnqueueRawWithKey(claim, key, func(ctx context.Context, obj any) error {
			done, res := d.nodePrepareResource(ctx, claim)
			if done {
				results[claim.UID] = res
				wg.Done()
				return nil
			}
			return fmt.Errorf("%w", res.Err)
		})
	}

	go func() {
		wg.Wait()
		cancel()
	}()

	workQueue.Run(ctx)
	return results, nil
}

func (d *driver) UnprepareResourceClaims(ctx context.Context, claimRefs []kubeletplugin.NamespacedObject) (map[types.UID]error, error) {
	klog.V(6).Infof("UnprepareResourceClaims called with %d claim(s)", len(claimRefs))

	var wg sync.WaitGroup
	ctx, cancel := context.WithTimeout(ctx, ErrorRetryMaxTimeout)

	// Review: do we want to have a new queue per incoming Prepare/Unprepare
	// request?
	workQueue := workqueue.New(workqueue.DefaultPrepUnprepRateLimiter())
	results := make(map[types.UID]error)

	for _, claim := range claimRefs {
		wg.Add(1)
		key := fmt.Sprintf("%v", claim.UID)
		workQueue.EnqueueRawWithKey(claim, key, func(ctx context.Context, obj any) error {
			done, err := d.nodeUnprepareResource(ctx, claim)
			if done {
				results[claim.UID] = err
				wg.Done()
				if err != nil {
					klog.V(0).Infof("Permanent error unpreparing devices for claim %v: %v", claim.UID, err)
				}
				return nil
			}
			return fmt.Errorf("%w", err)
		})
	}

	go func() {
		wg.Wait()
		cancel()
	}()

	workQueue.Run(ctx)

	return results, nil
}

func (d *driver) HandleError(ctx context.Context, err error, msg string) {
	// For now we just follow the advice documented in the DRAPlugin API docs.
	// See: https://pkg.go.dev/k8s.io/apimachinery/pkg/util/runtime#HandleErrorWithContext
	runtime.HandleErrorWithContext(ctx, err, msg)
}

// nodePrepareResource() returns a 2-tuple; the first value is a boolean
// indicating whether the work is 'done', the second value is a result which can
// also reflect an error. Set the boolean to `true` for any result wrapping a
// non-retryable error.
func (d *driver) nodePrepareResource(ctx context.Context, claim *resourceapi.ResourceClaim) (bool, kubeletplugin.PrepareResult) {
	t0 := time.Now()

	release, err := d.pulock.Acquire(ctx, flock.WithTimeout(10*time.Second))
	if err != nil {
		drametrics.IncNodePrepareError(DriverName, "lock_acquire")
		res := kubeletplugin.PrepareResult{
			Err: fmt.Errorf("error acquiring prep/unprep lock: %w", err),
		}
		return false, res
	}
	defer release()
	doneInFlight := drametrics.TrackInFlight(DriverName, "prepare")
	defer doneInFlight()

	if claim.Status.Allocation == nil {
		res := kubeletplugin.PrepareResult{
			Err: fmt.Errorf("no allocation set in ResourceClaim %s in namespace %s", claim.Name, claim.Namespace),
		}
		return true, res
	}

	devs, err := d.state.Prepare(ctx, claim)
	if err != nil {
		drametrics.IncNodePrepareError(DriverName, "prepare_devices")
		res := kubeletplugin.PrepareResult{
			Err: fmt.Errorf("error preparing devices for claim '%s': %w", ResourceClaimToString(claim), err),
		}
		if isPermanentError(err) {
			klog.Infof("Permanent error preparing devices for claim %v: %v", claim.UID, err)
			return true, res
		}
		return false, res
	}

	klog.V(1).Infof("Prepared devices for claim '%s': %v", ResourceClaimToString(claim), devs)
	drametrics.ObserveRequest(DriverName, "prepare", time.Since(t0))

	return true, kubeletplugin.PrepareResult{Devices: devs}
}

// Return 2-tuple: the first value is a boolean indicating to the retry logic
// whether the work is 'done'.
func (d *driver) nodeUnprepareResource(ctx context.Context, claimRef kubeletplugin.NamespacedObject) (bool, error) {
	tstart := time.Now()

	release, err := d.pulock.Acquire(ctx, flock.WithTimeout(10*time.Second))
	if err != nil {
		drametrics.IncNodeUnprepareError(DriverName, "lock_acquire")
		return false, fmt.Errorf("error acquiring prep/unprep lock: %w", err)
	}
	defer release()
	doneInFlight := drametrics.TrackInFlight(DriverName, "unprepare")
	defer doneInFlight()

	if err := d.state.Unprepare(ctx, claimRef); err != nil {
		drametrics.IncNodeUnprepareError(DriverName, "unprepare_devices")
		return isPermanentError(err), fmt.Errorf("error unpreparing devices for claim '%v': %w", claimRef.String(), err)
	}

	klog.V(1).Infof("Unprepared devices for claim '%v'", claimRef.String())
	drametrics.ObserveRequest(DriverName, "unprepare", time.Since(tstart))
	return true, nil
}

func (d *driver) deleteExpiredPrepareAbortedClaimEntries(ctx context.Context, now time.Time, ttl time.Duration) (int, error) {
	release, err := d.pulock.Acquire(ctx, flock.WithTimeout(10*time.Second))
	if err != nil {
		return 0, fmt.Errorf("error acquiring prep/unprep lock: %w", err)
	}
	defer release()

	return d.state.deleteExpiredPrepareAbortedClaimsFromCheckpoint(now, ttl)
}

// TODO: implement loop to remove CDI files from the CDI path for claimUIDs
//       that have been removed from the AllocatedClaims map.
// func (d *driver) cleanupCDIFiles(wg *sync.WaitGroup) chan error {
// 	errors := make(chan error)
// 	return errors
// }
