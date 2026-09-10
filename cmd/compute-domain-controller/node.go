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
	"strings"
	"sync"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
)

type NodeManager struct {
	config              *ManagerConfig
	waitGroup           sync.WaitGroup
	cancelContext       context.CancelFunc
	factory             informers.SharedInformerFactory
	informer            cache.SharedIndexInformer
	labelCleanupManager *CleanupManager[*corev1.Node]
}

func NewNodeManager(config *ManagerConfig, getComputeDomain GetComputeDomainFunc) *NodeManager {
	labelSelector := &metav1.LabelSelector{
		MatchExpressions: []metav1.LabelSelectorRequirement{
			{
				Key:      computeDomainLabelKey,
				Operator: metav1.LabelSelectorOpExists,
			},
		},
	}

	factory := informers.NewSharedInformerFactoryWithOptions(
		config.clientsets.Core,
		informerResyncPeriod,
		informers.WithTweakListOptions(func(opts *metav1.ListOptions) {
			opts.LabelSelector = metav1.FormatLabelSelector(labelSelector)
		}),
	)
	informer := factory.Core().V1().Nodes().Informer()

	m := &NodeManager{
		config:   config,
		factory:  factory,
		informer: informer,
	}

	m.labelCleanupManager = NewCleanupManager[*corev1.Node](informer, getComputeDomain, m.cleanupLabels)
	return m
}

func (m *NodeManager) Start(ctx context.Context) (rerr error) {
	ctx, cancel := context.WithCancel(ctx)
	m.cancelContext = cancel

	defer func() {
		if rerr != nil {
			if err := m.Stop(); err != nil {
				klog.Errorf("error stopping Node manager: %v", err)
			}
		}
	}()

	m.waitGroup.Add(1)
	go func() {
		defer m.waitGroup.Done()
		m.factory.Start(ctx.Done())
	}()

	if !cache.WaitForCacheSync(ctx.Done(), m.informer.HasSynced) {
		return fmt.Errorf("NodeManager: informer cache sync failed")
	}

	if err := m.labelCleanupManager.Start(ctx); err != nil {
		return fmt.Errorf("NodeManager: error starting labelCleanupManager: %w", err)
	}

	return nil
}

func (m *NodeManager) Stop() error {
	if err := m.labelCleanupManager.Stop(); err != nil {
		return fmt.Errorf("NodeManager: error stopping labelCleanupManager: %w", err)
	}
	if m.cancelContext != nil {
		m.cancelContext()
	}
	m.waitGroup.Wait()
	return nil
}

// RemoveComputeDomainLabels() searches all nodes that are currently labeled for
// a specific CD (as identified via CD UID). It then removes that label from all
// such nodes.
func (m *NodeManager) RemoveComputeDomainLabels(ctx context.Context, cdUID string) error {
	labelSelector := &metav1.LabelSelector{
		MatchExpressions: []metav1.LabelSelectorRequirement{
			{
				Key:      computeDomainLabelKey,
				Operator: metav1.LabelSelectorOpIn,
				Values:   []string{cdUID},
			},
		},
	}

	nodes, err := m.config.clientsets.Core.CoreV1().Nodes().List(ctx, metav1.ListOptions{
		LabelSelector: metav1.FormatLabelSelector(labelSelector),
	})
	if err != nil {
		return fmt.Errorf("error retrieving nodes: %w", err)
	}

	var names []string
	for _, node := range nodes.Items {
		// Rely on above's List() API call to only return node objects that
		// really have this label set. Remove it.
		newNode := node.DeepCopy()
		delete(newNode.Labels, computeDomainLabelKey)
		if _, err := m.config.clientsets.Core.CoreV1().Nodes().Update(ctx, newNode, metav1.UpdateOptions{}); err != nil {
			return fmt.Errorf("error updating node %s: %w", newNode.Name, err)
		}
		names = append(names, newNode.Name)
	}

	if len(names) > 0 {
		klog.V(1).Infof("Removed label(s) for CD %v from node(s): %v", cdUID, strings.Join(names, ", "))
	}

	return nil
}

// RemoveStaleCDLabelsAsync queues up at most 1 cleanup task (otherwise, this
// cleanup would be invoked unnecessarily often; this is relevant during
// controller startup or when CDs get created at high-ish frequency). This type
// of cleanup is important because workload may have been created/deleted in
// fast succession so that some CD node labels may have been applied after
// RemoveNodeLabels() was called for that CD (as part of regular CD deletion).
// That can result in dangling CD node labels (blocking affected nodes from
// being re-used for another CD).
func (m *NodeManager) RemoveStaleComputeDomainLabelsAsync(ctx context.Context) bool {
	return m.labelCleanupManager.EnqueueCleanup()
}

func (m *NodeManager) cleanupLabels(ctx context.Context, cdUID string) error {
	if err := m.RemoveComputeDomainLabels(ctx, cdUID); err != nil {
		return fmt.Errorf("error removing ComputeDomain node labels: %w", err)
	}
	return nil
}
