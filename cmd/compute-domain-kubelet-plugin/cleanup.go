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
	"sync"
	"time"

	resourcev1 "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	draclient "k8s.io/dynamic-resource-allocation/client"
	"k8s.io/dynamic-resource-allocation/kubeletplugin"
	"k8s.io/klog/v2"
)

const (
	ResourceClaimCleanupInterval = 10 * time.Minute
	PrepareAbortedClaimEntryTTL  = 2 * ErrorRetryMaxTimeout
)

type TypeUnprepCallable = func(ctx context.Context, claimRef kubeletplugin.NamespacedObject) (bool, error)
type TypeExpiredEntryCleanupCallable = func(ctx context.Context, now time.Time, ttl time.Duration) (int, error)

type CheckpointCleanupManager struct {
	waitGroup     sync.WaitGroup
	cancelContext context.CancelFunc
	queue         chan struct{}
	devicestate   *DeviceState
	draclient     *draclient.Client

	unprepfunc            TypeUnprepCallable
	expiredEntryCleanupFn TypeExpiredEntryCleanupCallable
}

func NewCheckpointCleanupManager(s *DeviceState, client *draclient.Client) *CheckpointCleanupManager {
	// `queue`: buffered channel to implement a pragmatic fixed-size queue; to
	// ensure at most one cleanup operation gets enqueued. TODO: review
	// replacing this with a condition variable.
	return &CheckpointCleanupManager{
		devicestate: s,
		draclient:   client,
		queue:       make(chan struct{}, 1),
	}
}

func (m *CheckpointCleanupManager) Start(ctx context.Context, unprepfunc TypeUnprepCallable, expiredEntryCleanupFn TypeExpiredEntryCleanupCallable) error {
	ctx, cancel := context.WithCancel(ctx)
	m.cancelContext = cancel
	m.unprepfunc = unprepfunc
	m.expiredEntryCleanupFn = expiredEntryCleanupFn

	m.waitGroup.Add(1)
	go func() {
		defer m.waitGroup.Done()
		// Start producer: periodically submit cleanup task.
		m.triggerPeriodically(ctx)
	}()

	m.waitGroup.Add(1)
	go func() {
		defer m.waitGroup.Done()
		// Start consumer
		m.worker(ctx)
	}()

	klog.V(6).Infof("CheckpointCleanupManager started")
	return nil
}

func (m *CheckpointCleanupManager) Stop() error {
	if m.cancelContext != nil {
		m.cancelContext()
	}
	m.waitGroup.Wait()
	return nil
}

// cleanup() is the high-level cleanup routine run once upon plugin startup and
// then periodically. It gets all claims in PrepareStarted state from the
// current checkpoint, and runs `unprepareIfStale()` for each of them. It also
// removes expired PrepareAborted entries. Each invocation of `cleanup()`
// and each invocation of `unprepareIfStale()` is best-effort: errors do not
// need to be propagated (but are expected to be properly logged).
//
// Note: This function does not acquire DeviceState lock when reading the checkpoint.
// The lock is not needed because:
//  1. This initial checkpoint read is just discovering candidate claims to check - it
//     doesn't need strong consistency. Missing a claim due to concurrent write just
//     means we catch it on the next periodic iteration.
//  2. We validate each candidate against the API server (the authoritative source of
//     truth for whether a claim is truly stale), not the local checkpoint state.
//  3. The actual checkpoint mutations happen in driver callbacks which properly
//     acquire the pulock (process-level file lock) for atomic read-modify-write.
//  4. Holding DeviceState lock during the entire cleanup (which could take seconds with
//     multiple API calls) would unnecessarily block normal Prepare/Unprepare operations.
func (m *CheckpointCleanupManager) cleanup(ctx context.Context) {
	cp, err := m.devicestate.getCheckpoint()
	if err != nil {
		klog.Errorf("Checkpointed RC cleanup: unable to get checkpoint: %s", err)
		return
	}

	// Get checkpointed claims in PrepareStarted state.
	filtered := make(PreparedClaimsByUIDV2)
	for uid, claim := range cp.V2.PreparedClaims {
		if claim.CheckpointState == ClaimCheckpointStatePrepareStarted {
			filtered[uid] = claim
		}
	}

	klog.V(4).Infof("Checkpointed RC cleanup: claims in PrepareStarted state: %d (of %d)", len(filtered), len(cp.V2.PreparedClaims))

	for cpuid, cpclaim := range filtered {
		m.unprepareIfStale(ctx, cpuid, cpclaim)
	}

	expiredEntries, err := m.expiredEntryCleanupFn(ctx, time.Now(), PrepareAbortedClaimEntryTTL)
	if err != nil {
		klog.Warningf("Checkpointed RC cleanup: unable to delete expired PrepareAborted claim entries: %s", err)
		return
	}
	if expiredEntries > 0 {
		klog.V(4).Infof("Checkpointed RC cleanup: deleted expired PrepareAborted claim entries: %d", expiredEntries)
	}
}

// Detect if claim is stale (not known to the API server). Call unprepare() if
// it is stale.
//
// There are two options to look up a claim with a specific UID from the API
// server:
//
//  1. List(), with `FieldSelector: "metadata.uid=<uid>"`. Especially
//     across all namespaces (but also within one namespace) this is
//     irresponsibly expensive lookup, cannot be done on a routine
//     basis.
//
//  2. Get(), using a specific name/namespace + subsequent UID
//     comparison. This is a cheap lookup for the API server.
//
// For (2), name and namespace must be stored in the checkpoint. That is not
// true for legacy deployments with checkpoint data created by version 25.3.x of
// this driver. Detect that situation by looking for an empty `Name`.
func (m *CheckpointCleanupManager) unprepareIfStale(ctx context.Context, cpuid string, cpclaim PreparedClaim) {
	if cpclaim.Name == "" {
		klog.V(4).Infof("Checkpointed RC cleanup: skip checkpointed claim '%s': RC name not in checkpoint", cpuid)
		// Consider fallback: expensive lookup by UID across all namespaces.
		return
	}

	claim, err := m.getClaimByName(ctx, cpclaim.Name, cpclaim.Namespace)
	if err != nil && errors.IsNotFound(err) {
		klog.V(4).Infof(
			"Checkpointed RC cleanup: partially prepared claim '%s/%s:%s' is stale: not found in API server",
			cpclaim.Namespace,
			cpclaim.Name,
			cpuid)
		m.unprepare(ctx, cpuid, cpclaim)
		return
	}

	// A transient error during API server lookup. No explicit retry required.
	// The next periodic cleanup invocation will implicitly retry.
	if err != nil {
		klog.V(1).Infof("Checkpointed RC cleanup: skip for checkpointed claim %s: getClaimByName failed (retry later): %s", cpuid, err)
		return
	}

	if string(claim.UID) != cpuid {
		// There cannot be two ResourceClaim objects with the same name in
		// the same namespace at the same time. It is possible for a
		// ResourceClaim with the same name to have a different UID if the
		// original object was deleted and a new one with the same name was
		// created. Hence, this checkpointed claim is stale.
		klog.V(4).Infof("Checkpointed RC cleanup: partially prepared claim '%s/%s' is stale: UID changed (checkpoint: %s, API server: %s)", cpclaim.Namespace, cpclaim.Name, cpuid, claim.UID)
		m.unprepare(ctx, cpuid, cpclaim)
		return
	}

	klog.V(4).Infof("Checkpointed RC cleanup: partially prepared claim not stale: %s", ResourceClaimToString(claim))
}

// unprepare() attempts to unprepare devices for the provided claim
// ('self-initiated unprepare'). Expected side effect: removal of the
// corresponding claim from the checkpoint.
func (m *CheckpointCleanupManager) unprepare(ctx context.Context, uid string, claim PreparedClaim) {
	claimRef := kubeletplugin.NamespacedObject{
		UID: types.UID(uid),
		NamespacedName: types.NamespacedName{
			Name:      claim.Name,
			Namespace: claim.Namespace,
		},
	}

	// Perform one Unprepare attempt. Implicit retrying across periodic cleanup
	// invocations is sufficient. Rely on Unprepare() to delete claim from
	// checkpoint (upon success). TODO: review `Unprepare()` for code paths that
	// allow for this claim never to be dropped from the checkpoint (resulting
	// in infinite periodic cleanup attempts for this claim).
	_, err := m.unprepfunc(ctx, claimRef)
	if err != nil {
		klog.Warningf("Checkpointed RC cleanup: error during unprepare for %s (retried later): %s", claimRef.String(), err)
		return
	}

	klog.V(1).Infof("Checkpointed RC cleanup: unprepared stale claim: %s", claimRef.String())
}

// getClaimByName() attempts to fetch a ResourceClaim object directly from the
// API server.
func (m *CheckpointCleanupManager) getClaimByName(ctx context.Context, name string, ns string) (*resourcev1.ResourceClaim, error) {
	// The API call below should be responded to with low latency. Choose a
	// timeout constant here that reflects a pathological state if met; in this
	// case give up.
	childctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	claim, err := m.draclient.ResourceClaims(ns).Get(childctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("error getting resource claim %s/%s: %w", name, ns, err)
	}

	return claim, nil
}

// enqueueCleanup() submits a cleanup task if the queue is currently empty.
// Return a Boolean indicating whether the task was submitted or not.
func (m *CheckpointCleanupManager) enqueueCleanup() bool {
	select {
	case m.queue <- struct{}{}:
		return true
	default:
		// Channel full: one task already lined up, did not submit more.
		return false
	}
}

// Run forever until context is canceled.
func (m *CheckpointCleanupManager) worker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-m.queue:
			// Do we want to timeout-control this cleanup run? What may take
			// unexpectedly long: lock acquisition (if we do any, e.g. around
			// checkpoint file mutation), API server interaction.
			m.cleanup(ctx)
		}
	}
}

// Immediately submit a cleanup task; then periodically submit cleanup tasks
// forever.
func (m *CheckpointCleanupManager) triggerPeriodically(ctx context.Context) {
	// Maybe add jitter. Or delay first cleanup by a somewhat random amount.
	// After all, this periodic cleanup runs in N kubelet plugins and upon
	// driver upgrade they might restart at roughly the same time -- it makes
	// sense to smear the API server load out over time.
	ticker := time.NewTicker(ResourceClaimCleanupInterval)
	defer ticker.Stop()

	m.cleanup(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if m.enqueueCleanup() {
				klog.V(6).Infof("Checkpointed RC cleanup: task submitted")
			} else {
				// A previous cleanup is taking long; that may not be normal.
				klog.Warningf("Checkpointed RC cleanup: ongoing, skipped")
			}
		}
	}
}
