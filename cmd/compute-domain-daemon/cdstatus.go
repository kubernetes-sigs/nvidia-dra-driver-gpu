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
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"

	nvapi "sigs.k8s.io/dra-driver-nvidia-gpu/api/nvidia.com/resource/v1beta1"
	"sigs.k8s.io/dra-driver-nvidia-gpu/pkg/featuregates"
	nvinformers "sigs.k8s.io/dra-driver-nvidia-gpu/pkg/nvidia.com/informers/externalversions"
)

// ComputeDomainStatusManager watches compute domains and updates their status with
// info about the ComputeDomain daemon running on this node.
type ComputeDomainStatusManager struct {
	config        *ManagerConfig
	waitGroup     sync.WaitGroup
	cancelContext context.CancelFunc

	factory  nvinformers.SharedInformerFactory
	informer cache.SharedIndexInformer

	// Note: if `previousNodes` is empty it means we're early in the daemon's
	// lifecycle and the IMEX daemon child process wasn't started yet.
	previousNodes      []*nvapi.ComputeDomainNode
	updatedDaemonsChan chan []*nvapi.ComputeDomainDaemonInfo

	podManager    *PodManager
	mutationCache cache.MutationCache
}

// NewComputeDomainStatusManager creates a new ComputeDomainStatusManager instance.
func NewComputeDomainStatusManager(config *ManagerConfig) *ComputeDomainStatusManager {
	m := &ComputeDomainStatusManager{
		config:             config,
		previousNodes:      []*nvapi.ComputeDomainNode{},
		updatedDaemonsChan: make(chan []*nvapi.ComputeDomainDaemonInfo),
	}

	m.factory = nvinformers.NewSharedInformerFactoryWithOptions(
		config.clientsets.Nvidia,
		informerResyncPeriod,
		nvinformers.WithNamespace(config.computeDomainNamespace),
		nvinformers.WithTweakListOptions(func(opts *metav1.ListOptions) {
			opts.FieldSelector = fmt.Sprintf("metadata.name=%s", config.computeDomainName)
		}),
	)
	m.informer = m.factory.Resource().V1beta1().ComputeDomains().Informer()

	m.podManager = NewPodManager(m.config, m.updateNodeStatus)

	return m
}

// Start starts the compute domain manager.
func (m *ComputeDomainStatusManager) Start(ctx context.Context) (rerr error) {
	ctx, cancel := context.WithCancel(ctx)
	m.cancelContext = cancel

	defer func() {
		if rerr != nil {
			if err := m.Stop(); err != nil {
				klog.Errorf("error stopping ComputeDomainStatusManager: %v", err)
			}
		}
	}()

	err := m.informer.AddIndexers(cache.Indexers{
		"uid": uidIndexer[*nvapi.ComputeDomain],
	})
	if err != nil {
		return fmt.Errorf("error adding indexer for ComputeDomain UID: %w", err)
	}

	// Create mutation cache to track our own updates
	m.mutationCache = cache.NewIntegerResourceVersionMutationCache(
		klog.Background(),
		m.informer.GetStore(),
		m.informer.GetIndexer(),
		mutationCacheTTL,
		true,
	)

	// Use `WithKey` with hard-coded key, to cancel any previous update task (we
	// want to make sure that the latest CD status update wins).
	_, err = m.informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj any) {
			m.config.workQueue.EnqueueWithKey(obj, "cd", m.onAddOrUpdate)
		},
		UpdateFunc: func(objOld, objNew any) {
			m.config.workQueue.EnqueueWithKey(objNew, "cd", m.onAddOrUpdate)
		},
	})
	if err != nil {
		return fmt.Errorf("error adding event handlers for ComputeDomain informer: %w", err)
	}

	m.waitGroup.Add(1)
	go func() {
		defer m.waitGroup.Done()
		m.factory.Start(ctx.Done())
	}()

	if !cache.WaitForCacheSync(ctx.Done(), m.informer.HasSynced) {
		return fmt.Errorf("informer cache sync for ComputeDomains failed")
	}

	if err := m.podManager.Start(ctx); err != nil {
		return fmt.Errorf("failed to start pod manager: %w", err)
	}

	return nil
}

// Stop stops the compute domain manager.
//
//nolint:contextcheck
func (m *ComputeDomainStatusManager) Stop() error {
	// Stop the pod manager first
	if err := m.podManager.Stop(); err != nil {
		klog.Errorf("Failed to stop pod manager: %v", err)
	}

	// Create a new context for cleanup operations since the original context might be cancelled
	cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cleanupCancel()

	// Attempt to remove this node from the ComputeDomain status before shutting down
	// Don't return error here as we still want to proceed with shutdown
	if err := m.removeNodeFromComputeDomain(cleanupCtx); err != nil {
		klog.Errorf("Failed to remove node from ComputeDomain during shutdown: %v", err)
	}

	if m.cancelContext != nil {
		m.cancelContext()
	}

	m.waitGroup.Wait()
	return nil
}

// Get gets the ComputeDomain by UID from the mutation cache.
func (m *ComputeDomainStatusManager) Get(uid string) (*nvapi.ComputeDomain, error) {
	cds, err := getByComputeDomainUID[*nvapi.ComputeDomain](m.mutationCache, uid)
	if err != nil {
		return nil, fmt.Errorf("error retrieving ComputeDomain by UID: %w", err)
	}
	if len(cds) == 0 {
		return nil, nil
	}
	if len(cds) != 1 {
		return nil, fmt.Errorf("multiple ComputeDomains with the same UID")
	}
	return cds[0], nil
}

// onAddOrUpdate handles the addition or update of a ComputeDomain. Here, we
// receive updates not for all CDs in the system, but only for the CD that we
// are registered for (filtered by CD name). Note that the informer triggers
// this callback once upon startup for all existing objects.
func (m *ComputeDomainStatusManager) onAddOrUpdate(ctx context.Context, obj any) error {
	// Cast the object to a ComputeDomain object
	o, ok := obj.(*nvapi.ComputeDomain)
	if !ok {
		return fmt.Errorf("failed to cast to ComputeDomain")
	}

	// Get the latest ComputeDomain object from the mutation cache (backed by
	// the informer cache) since we plan to update it later and always *must*
	// have the latest version.
	cd, err := m.Get(string(o.GetUID()))
	if err != nil {
		return fmt.Errorf("error getting latest ComputeDomain: %w", err)
	}
	if cd == nil {
		return nil
	}

	// Because the informer only filters by name:
	// Skip ComputeDomains that don't match on UUID
	if string(cd.UID) != m.config.computeDomainUUID {
		klog.Warningf("ComputeDomain processed with non-matching UID (%v, %v)", cd.UID, m.config.computeDomainUUID)
		return nil
	}

	// Update node info in ComputeDomain, if required.
	cd, err = m.syncNodeInfoToCD(ctx, cd)
	if err != nil {
		return fmt.Errorf("CD update: failed to insert/update node info in CD: %w", err)
	}
	m.maybePushNodesUpdate(cd)

	return nil
}

// syncNodeInfoToCD makes sure that the current node (by node name) is
// represented in the `Nodes` field in the ComputeDomain object, and that it
// reports the IP address of this current pod running the CD daemon. If mutation
// is needed (first insertion, or IP address update) and successful, it reflects
// the mutation in `m.mutationCache`.
func (m *ComputeDomainStatusManager) syncNodeInfoToCD(ctx context.Context, cd *nvapi.ComputeDomain) (*nvapi.ComputeDomain, error) {
	var myNode *nvapi.ComputeDomainNode

	// Create a deep copy of the ComputeDomain to avoid modifying the original
	newCD := cd.DeepCopy()

	// Try to find an existing entry for the current k8s node
	for _, node := range newCD.Status.Nodes {
		if node.Name == m.config.nodeName {
			myNode = node
			break
		}
	}

	// If there is one and its IP is the same as this one, we are done
	if myNode != nil && myNode.IPAddress == m.config.podIP {
		klog.V(6).Infof("syncNodeInfoToCD noop: pod IP unchanged (%s)", m.config.podIP)
		return newCD, nil
	}

	// If there isn't one, create one and append it to the list
	if myNode == nil {
		// Get the next available index for this new node
		nextIndex, err := m.getNextAvailableIndex(newCD.Status.Nodes)
		if err != nil {
			return nil, fmt.Errorf("error getting next available index: %w", err)
		}

		myNode = &nvapi.ComputeDomainNode{
			Name:     m.config.nodeName,
			CliqueID: m.config.cliqueID,
			Index:    nextIndex,
			// This is going to be switched to Ready by podmanager.
			Status: nvapi.ComputeDomainStatusNotReady,
		}

		klog.Infof("CD status does not contain node name '%s' yet, try to insert myself: %v", m.config.nodeName, myNode)
		newCD.Status.Nodes = append(newCD.Status.Nodes, myNode)
	}

	// Unconditionally update its IP address. Note that the myNode.IPAddress
	// as of now translates into a pod IP address and may therefore change
	// across pod restarts.
	myNode.IPAddress = m.config.podIP

	// Update status and (upon success) store the latest version of the object
	// (as returned by the API server) in the mutation cache.
	newCD, err := m.config.clientsets.Nvidia.ResourceV1beta1().ComputeDomains(newCD.Namespace).UpdateStatus(ctx, newCD, metav1.UpdateOptions{})
	if err != nil {
		return nil, fmt.Errorf("error updating ComputeDomain status: %w", err)
	}
	m.mutationCache.Mutation(newCD)

	klog.Infof("Successfully inserted/updated node in CD (nodeinfo: %v)", myNode)
	return newCD, nil
}

// The Index field in the Nodes section of the ComputeDomain status ensures a
// consistent IP-to-DNS name mapping across all machines within a given IMEX
// domain. Each node's index directly determines its DNS name using the format
// "compute-domain-daemon-{index}".
//
// getNextAvailableIndex finds the next available index for the current node by
// seeing which ones are already taken by other nodes in the ComputeDomain
// status that have the same cliqueID. It fills in gaps where it can, and returns
// an error if no index is available within maxNodesPerIMEXDomain.
//
// By filling gaps in the index sequence (rather than always appending), we
// maintain stable DNS names for existing nodes even when intermediate nodes
// are removed from the compute domain and new ones are added.
func (m *ComputeDomainStatusManager) getNextAvailableIndex(nodes []*nvapi.ComputeDomainNode) (int, error) {
	// Filter nodes to only consider those with the same cliqueID
	var cliqueNodes []*nvapi.ComputeDomainNode
	for _, node := range nodes {
		if node.CliqueID == m.config.cliqueID {
			cliqueNodes = append(cliqueNodes, node)
		}
	}

	// Create a map to track used indices
	usedIndices := make(map[int]bool)

	// Collect all currently used indices from nodes with the same cliqueID
	for _, node := range cliqueNodes {
		usedIndices[node.Index] = true
	}

	// Find the next available index, starting from 0 and filling gaps
	nextIndex := 0
	for usedIndices[nextIndex] {
		nextIndex++
	}

	// Skip `maxNodesPerIMEXDomain` check in the special case of no clique ID
	// being set: this means that this node does not actually run an IMEX daemon
	// managed by us and the set of nodes in this "noop" mode in this CD is
	// allowed to grow larger than maxNodesPerIMEXDomain.
	if m.config.cliqueID == "" {
		return nextIndex, nil
	}

	// Ensure nextIndex is within the range 0..maxNodesPerIMEXDomain
	if nextIndex < 0 || nextIndex >= m.config.maxNodesPerIMEXDomain {
		return -1, fmt.Errorf("no available indices within maxNodesPerIMEXDomain (%d) for cliqueID %s", m.config.maxNodesPerIMEXDomain, m.config.cliqueID)
	}

	return nextIndex, nil
}

// If there was actually a change compared to the previously known set of
// nodes: pass info to IMEX daemon controller.
func (m *ComputeDomainStatusManager) maybePushNodesUpdate(cd *nvapi.ComputeDomain) {
	// When not running with the 'IMEXDaemonsWithDNSNames' feature enabled,
	// wait for all 'numNodes' nodes to show up before sending an update.
	if !featuregates.Enabled(featuregates.IMEXDaemonsWithDNSNames) {
		if len(cd.Status.Nodes) != cd.Spec.NumNodes {
			klog.V(1).Infof("numNodes: %d, nodes seen: %d", cd.Spec.NumNodes, len(cd.Status.Nodes))
			return
		}
	}

	newIPs := m.getIPSetForClique(cd.Status.Nodes)
	previousIPs := m.getIPSetForClique(m.previousNodes)

	// Compare sets (without paying attention to order).
	if !maps.Equal(newIPs, previousIPs) {
		added, removed := previousIPs.Diff(newIPs)
		klog.V(2).Infof("IP set for clique changed.\nAdded: %v\nRemoved: %v", added, removed)
		m.previousNodes = cd.Status.Nodes
		m.updatedDaemonsChan <- m.nodesToDaemonInfos(cd.Status.Nodes)
	} else {
		klog.V(6).Infof("IP set for clique did not change")
	}
}

// nodesToDaemonInfos converts ComputeDomainNodes to ComputeDomainDaemonInfos.
func (m *ComputeDomainStatusManager) nodesToDaemonInfos(nodes []*nvapi.ComputeDomainNode) []*nvapi.ComputeDomainDaemonInfo {
	daemons := make([]*nvapi.ComputeDomainDaemonInfo, len(nodes))
	for i, node := range nodes {
		daemons[i] = &nvapi.ComputeDomainDaemonInfo{
			NodeName:  node.Name,
			IPAddress: node.IPAddress,
			CliqueID:  node.CliqueID,
			Index:     node.Index,
			Status:    node.Status,
		}
	}
	return daemons
}

// GetDaemonInfoUpdateChan returns the channel that yields daemon info updates.
// Updates are only a complete set (size `numNodes`) if IMEXDaemonsWithDNSNames=false.
func (m *ComputeDomainStatusManager) GetDaemonInfoUpdateChan() chan []*nvapi.ComputeDomainDaemonInfo {
	return m.updatedDaemonsChan
}

// removeNodeFromComputeDomain removes the current node's entry from the ComputeDomain status.
func (m *ComputeDomainStatusManager) removeNodeFromComputeDomain(ctx context.Context) error {
	cd, err := m.Get(m.config.computeDomainUUID)
	if err != nil {
		return fmt.Errorf("error getting ComputeDomain from mutation cache: %w", err)
	}
	if cd == nil {
		klog.Infof("No ComputeDomain object found in mutation cache during cleanup")
		return nil
	}

	newCD := cd.DeepCopy()

	// Filter out the node with the current pod's IP address
	var updatedNodes []*nvapi.ComputeDomainNode
	for _, node := range newCD.Status.Nodes {
		if node.IPAddress != m.config.podIP {
			updatedNodes = append(updatedNodes, node)
		}
	}

	// Exit early if no nodes were removed
	if len(updatedNodes) == len(newCD.Status.Nodes) {
		return nil
	}

	// Update status and (upon success) store the latest version of the object
	// (as returned by the API server) in the mutation cache.
	newCD.Status.Nodes = updatedNodes
	newCD, err = m.config.clientsets.Nvidia.ResourceV1beta1().ComputeDomains(newCD.Namespace).UpdateStatus(ctx, newCD, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("error removing node from ComputeDomain status: %w", err)
	}
	m.mutationCache.Mutation(newCD)

	klog.Infof("Successfully removed node with IP %s from ComputeDomain %s/%s", m.config.podIP, newCD.Namespace, newCD.Name)
	return nil
}

// updateNodeStatus updates the status of the current node in the CD status.
func (m *ComputeDomainStatusManager) updateNodeStatus(ctx context.Context, ready bool) error {
	status := nvapi.ComputeDomainStatusNotReady
	if ready {
		status = nvapi.ComputeDomainStatusReady
	}

	cd, err := m.Get(m.config.computeDomainUUID)
	if err != nil {
		return fmt.Errorf("failed to get ComputeDomain: %w", err)
	}
	if cd == nil {
		return fmt.Errorf("ComputeDomain '%s/%s' not found", m.config.computeDomainName, m.config.computeDomainUUID)
	}

	// Create a deep copy to modify
	newCD := cd.DeepCopy()

	// Find the node
	var myNode *nvapi.ComputeDomainNode
	for _, n := range newCD.Status.Nodes {
		if n.Name == m.config.nodeName {
			myNode = n
			break
		}
	}
	if myNode == nil {
		return fmt.Errorf("node not yet listed in CD (waiting for insertion)")
	}

	// If status hasn't changed, we're done
	if myNode.Status == status {
		klog.V(6).Infof("updateNodeStatus noop: status not changed (%s)", status)
		return nil
	}

	// Update the status
	myNode.Status = status

	// Update status and (upon success) store the latest version of the object
	// (as returned by the API server) in the mutation cache.
	newCD, err = m.config.clientsets.Nvidia.ResourceV1beta1().ComputeDomains(newCD.Namespace).UpdateStatus(ctx, newCD, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("error updating ComputeDomain status: %w", err)
	}
	m.mutationCache.Mutation(newCD)

	klog.Infof("Successfully updated node status in CD (new status: %s)", status)
	return nil
}

func (m *ComputeDomainStatusManager) getIPSetForClique(nodeInfos []*nvapi.ComputeDomainNode) IPSet {
	set := make(IPSet)
	for _, n := range nodeInfos {
		if n.CliqueID == m.config.cliqueID {
			set[n.IPAddress] = struct{}{}
		}
	}
	return set
}
