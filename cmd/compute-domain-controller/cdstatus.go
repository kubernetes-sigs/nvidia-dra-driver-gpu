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
	"slices"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"

	nvapi "sigs.k8s.io/dra-driver-nvidia-gpu/api/nvidia.com/resource/v1beta1"
	"sigs.k8s.io/dra-driver-nvidia-gpu/pkg/featuregates"
)

const (
	// cdStatusSyncInterval is how often to sync node info to CD status and clean up stale clique entries.
	cdStatusSyncInterval = 2 * time.Second
)

// ComputeDomainStatusManager synchronizes node information to ComputeDomain status from
// both CDCliques (fabric-attached nodes) and daemon pods (non-fabric-attached nodes).
type ComputeDomainStatusManager struct {
	config        *ManagerConfig
	waitGroup     sync.WaitGroup
	cancelContext context.CancelFunc

	cliqueManager *ComputeDomainCliqueManager
	podManager    *DaemonSetPodManager

	listComputeDomains        ListComputeDomainsFunc
	updateComputeDomainStatus UpdateComputeDomainStatusFunc
}

// NewComputeDomainStatusManager creates a new ComputeDomainStatusManager.
func NewComputeDomainStatusManager(config *ManagerConfig, listComputeDomains ListComputeDomainsFunc, updateComputeDomainStatus UpdateComputeDomainStatusFunc) *ComputeDomainStatusManager {
	// Create cliqueManager if feature gate is enabled
	var cliqueManager *ComputeDomainCliqueManager
	if featuregates.Enabled(featuregates.ComputeDomainCliques) {
		cliqueManager = NewComputeDomainCliqueManager(config)
	}

	// Create podManager
	podManager := NewDaemonSetPodManager(config)

	return &ComputeDomainStatusManager{
		config:                    config,
		cliqueManager:             cliqueManager,
		podManager:                podManager,
		listComputeDomains:        listComputeDomains,
		updateComputeDomainStatus: updateComputeDomainStatus,
	}
}

// Start starts the ComputeDomainStatusManager.
func (m *ComputeDomainStatusManager) Start(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	m.cancelContext = cancel

	// Start cliqueManager if it exists
	if m.cliqueManager != nil {
		if err := m.cliqueManager.Start(ctx); err != nil {
			return fmt.Errorf("error starting ComputeDomainClique manager: %w", err)
		}
	}

	// Start podManager
	if err := m.podManager.Start(ctx); err != nil {
		return fmt.Errorf("error starting DaemonSetPod manager: %w", err)
	}

	klog.Info("ComputeDomainStatusManager: starting periodic sync")

	// Start periodic sync loop (also handles clique cleanup when feature gate is enabled)
	m.waitGroup.Add(1)
	go func() {
		defer m.waitGroup.Done()
		m.startPeriodicSync(ctx)
	}()

	return nil
}

// Stop stops the ComputeDomainStatusManager.
func (m *ComputeDomainStatusManager) Stop() error {
	if err := m.podManager.Stop(); err != nil {
		klog.Errorf("error stopping DaemonSetPod manager: %v", err)
	}
	if m.cliqueManager != nil {
		if err := m.cliqueManager.Stop(); err != nil {
			klog.Errorf("error stopping ComputeDomainClique manager: %v", err)
		}
	}
	if m.cancelContext != nil {
		m.cancelContext()
	}
	m.waitGroup.Wait()
	return nil
}

// startPeriodicSync runs the sync every cdStatusSyncInterval.
func (m *ComputeDomainStatusManager) startPeriodicSync(ctx context.Context) {
	ticker := time.NewTicker(cdStatusSyncInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.sync(ctx)
		}
	}
}

// sync synchronizes node information to all ComputeDomain statuses.
func (m *ComputeDomainStatusManager) sync(ctx context.Context) {
	// Get all ComputeDomains
	cds, err := m.listComputeDomains()
	if err != nil {
		klog.Errorf("CDStatusSync: error listing ComputeDomains: %v", err)
		return
	}

	// Get all daemon pods
	pods, err := m.podManager.List()
	if err != nil {
		klog.Errorf("CDStatusSync: error listing pods: %v", err)
		return
	}

	// Get fabric-attached nodes from cliques (if feature gate is enabled)
	var cliques []*nvapi.ComputeDomainClique
	if m.cliqueManager != nil {
		cliques, err = m.cliqueManager.List()
		if err != nil {
			klog.Errorf("CDStatusSync: error listing cliques: %v", err)
			return
		}

		// Clean up stale entries from cliques in parallel
		for _, clique := range cliques {
			go m.cleanupClique(ctx, clique, pods)
		}
	}

	// Group cliques by CD UID
	cliquesByCD := make(map[string][]*nvapi.ComputeDomainClique)
	for _, clique := range cliques {
		cdUID := clique.Labels[computeDomainLabelKey]
		if cdUID == "" {
			continue
		}
		cliquesByCD[cdUID] = append(cliquesByCD[cdUID], clique)
	}

	// Group pods by CD UID and type (fabric-attached vs non-fabric-attached)
	fabricPodsByCD := make(map[string][]*corev1.Pod)
	nonFabricPodsByCD := make(map[string][]*corev1.Pod)
	for _, pod := range pods {
		cdUID := pod.Labels[computeDomainLabelKey]
		if cdUID == "" {
			continue
		}

		// Separate pods based on cliqueID label
		cliqueID, exists := pod.Labels[computeDomainCliqueLabelKey]
		if !exists || cliqueID != "" {
			// Unlabeled or fabric-attached: treat as fabric pods
			fabricPodsByCD[cdUID] = append(fabricPodsByCD[cdUID], pod)
		} else {
			// Explicitly empty cliqueID: non-fabric pods
			nonFabricPodsByCD[cdUID] = append(nonFabricPodsByCD[cdUID], pod)
		}
	}

	// Sync each CD in parallel
	var wg sync.WaitGroup
	for _, cd := range cds {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m.syncCD(ctx, cd, cliquesByCD[string(cd.UID)], fabricPodsByCD[string(cd.UID)], nonFabricPodsByCD[string(cd.UID)])
		}()
	}
	wg.Wait()
}

// syncCD synchronizes node information to a single ComputeDomain's status.
func (m *ComputeDomainStatusManager) syncCD(ctx context.Context, cd *nvapi.ComputeDomain, cliques []*nvapi.ComputeDomainClique, fabricPods []*corev1.Pod, nonFabricPods []*corev1.Pod) {
	var fabricNodes, nonFabricNodes, newNodes []*nvapi.ComputeDomainNode

	if m.cliqueManager != nil {
		// Feature gate enabled: build from cliques + non-fabric pods
		fabricNodes = m.buildNodesFromCliques(cliques)
		nonFabricNodes = m.buildNodesFromPods(nonFabricPods)
		newNodes = slices.Concat(fabricNodes, nonFabricNodes)
	} else {
		// Feature gate disabled: filter stale fabric nodes + rebuild non-fabric nodes
		fabricNodes = m.getNonStaleFabricNodes(cd.Status.Nodes, fabricPods)
		nonFabricNodes = m.buildNodesFromPods(nonFabricPods)
		newNodes = slices.Concat(fabricNodes, nonFabricNodes)
	}

	// Check if update is needed
	if m.nodesEqual(cd.Status.Nodes, newNodes) {
		return
	}

	klog.V(6).Infof("CDStatusSync: syncing ComputeDomain %s/%s: fabric=%d non-fabric=%d", cd.Namespace, cd.Name, len(fabricNodes), len(nonFabricNodes))

	// Update status
	newCD := cd.DeepCopy()
	newCD.Status.Nodes = newNodes
	if _, err := m.updateComputeDomainStatus(ctx, newCD); err != nil {
		klog.Errorf("CDStatusSync: error updating ComputeDomain %s/%s status: %v", cd.Namespace, cd.Name, err)
		return
	}

	klog.V(4).Infof("CDStatusSync: updated ComputeDomain %s/%s: total nodes=%d", cd.Namespace, cd.Name, len(newNodes))
}

// buildNodesFromCliques builds a nodes list from fabric-attached cliques.
func (m *ComputeDomainStatusManager) buildNodesFromCliques(cliques []*nvapi.ComputeDomainClique) []*nvapi.ComputeDomainNode {
	var result []*nvapi.ComputeDomainNode
	for _, clique := range cliques {
		for _, daemon := range clique.Daemons {
			result = append(result, &nvapi.ComputeDomainNode{
				Name:      daemon.NodeName,
				IPAddress: daemon.IPAddress,
				CliqueID:  daemon.CliqueID,
				Index:     daemon.Index,
				Status:    daemon.Status,
			})
		}
	}
	return result
}

// buildNodesFromPods builds ComputeDomainNode entries from non-fabric-attached pods.
func (m *ComputeDomainStatusManager) buildNodesFromPods(pods []*corev1.Pod) []*nvapi.ComputeDomainNode {
	var nodes []*nvapi.ComputeDomainNode
	for _, pod := range pods {
		if pod.Spec.NodeName == "" || pod.Status.PodIP == "" {
			continue
		}

		status := nvapi.ComputeDomainStatusNotReady
		for _, condition := range pod.Status.Conditions {
			if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
				status = nvapi.ComputeDomainStatusReady
				break
			}
		}

		nodes = append(nodes, &nvapi.ComputeDomainNode{
			Name:      pod.Spec.NodeName,
			IPAddress: pod.Status.PodIP,
			CliqueID:  "",
			Index:     -1,
			Status:    status,
		})
	}
	return nodes
}

// cleanupClique removes stale daemon entries from a single clique.
func (m *ComputeDomainStatusManager) cleanupClique(ctx context.Context, clique *nvapi.ComputeDomainClique, pods []*corev1.Pod) {
	// Build set of node names that have running daemon pods
	runningNodes := make(map[string]struct{})
	for _, pod := range pods {
		if pod.Spec.NodeName != "" {
			runningNodes[pod.Spec.NodeName] = struct{}{}
		}
	}

	var updatedDaemons []*nvapi.ComputeDomainDaemonInfo
	var removedNodes []string

	for _, daemon := range clique.Daemons {
		if _, exists := runningNodes[daemon.NodeName]; exists {
			updatedDaemons = append(updatedDaemons, daemon)
		} else {
			removedNodes = append(removedNodes, daemon.NodeName)
		}
	}

	// Nothing to clean up
	if len(removedNodes) == 0 {
		return
	}

	klog.V(1).Infof("CliqueCleanup: removing stale daemon entries from clique %s/%s: %v", clique.Namespace, clique.Name, removedNodes)

	// Update the clique with the filtered daemon list
	newClique := clique.DeepCopy()
	newClique.Daemons = updatedDaemons

	if _, err := m.cliqueManager.Update(ctx, newClique); err != nil {
		klog.Errorf("CliqueCleanup: error updating ComputeDomainClique %s/%s: %v", clique.Namespace, clique.Name, err)
		return
	}

	klog.V(1).Infof("CliqueCleanup: successfully removed %d stale daemon entries from clique %s/%s", len(removedNodes), clique.Namespace, clique.Name)
}

// filterStaleNodes removes nodes from CD status if their pod no longer exists.
// It filters the existing nodes list to only keep those with a corresponding pod in the pods list.
// getNonStaleFabricNodes returns fabric-attached nodes from existingNodes that still have running pods.
// Non-fabric nodes are filtered out (they'll be rebuilt from nonFabricPods).
func (m *ComputeDomainStatusManager) getNonStaleFabricNodes(existingNodes []*nvapi.ComputeDomainNode, fabricPods []*corev1.Pod) []*nvapi.ComputeDomainNode {
	// Build set of fabric pod IPs
	fabricPodIPs := make(map[string]struct{})
	for _, pod := range fabricPods {
		if pod.Status.PodIP != "" {
			fabricPodIPs[pod.Status.PodIP] = struct{}{}
		}
	}

	// Keep only fabric nodes (CliqueID != "") that still have pods
	var result []*nvapi.ComputeDomainNode
	for _, node := range existingNodes {
		// Skip non-fabric nodes (they're rebuilt fresh)
		if node.CliqueID == "" {
			continue
		}
		// Keep fabric node if its pod still exists
		if _, exists := fabricPodIPs[node.IPAddress]; exists {
			result = append(result, node)
		}
	}

	return result
}

// nodesEqual checks if two slices of ComputeDomainNode are equal.
func (m *ComputeDomainStatusManager) nodesEqual(a, b []*nvapi.ComputeDomainNode) bool {
	aMap := make(map[string]nvapi.ComputeDomainNode)
	for _, node := range a {
		aMap[node.Name] = *node
	}
	bMap := make(map[string]nvapi.ComputeDomainNode)
	for _, node := range b {
		bMap[node.Name] = *node
	}
	return maps.Equal(aMap, bMap)
}
