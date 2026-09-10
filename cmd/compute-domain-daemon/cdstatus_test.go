package main

import (
	"context"
	"fmt"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/cache"

	nvapi "sigs.k8s.io/dra-driver-nvidia-gpu/api/nvidia.com/resource/v1beta1"
	"sigs.k8s.io/dra-driver-nvidia-gpu/pkg/flags"
	nvfake "sigs.k8s.io/dra-driver-nvidia-gpu/pkg/nvidia.com/clientset/versioned/fake"
)

type noopMutationCache struct{}

func (n *noopMutationCache) GetByKey(string) (interface{}, bool, error) {
	return nil, false, nil
}

func (n *noopMutationCache) ByIndex(string, string) ([]interface{}, error) {
	return nil, nil
}

func (n *noopMutationCache) Mutation(interface{}) {}

var _ cache.MutationCache = &noopMutationCache{}

// This regression test simulates multiple daemon pods updating ComputeDomain
// status concurrently and verifies that all node entries are preserved in the status.
func TestSyncNodeInfoToCDPreservesExistingNodesAcrossConcurrentWriters(t *testing.T) {
	t.Parallel()
	t.Skip("tracked in GH issue #1050")

	const (
		namespace = "default"
		name      = "cd-foo"
		uid       = "cd-uid"
		cliqueID  = "clique-1"
	)

	for _, numWriters := range []int{2, 3, 5} {
		t.Run(fmt.Sprintf("writers=%d", numWriters), func(t *testing.T) {
			cd := &nvapi.ComputeDomain{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: namespace,
					Name:      name,
					UID:       uid,
				},
				Spec: nvapi.ComputeDomainSpec{
					NumNodes: numWriters,
				},
			}

			client := nvfake.NewSimpleClientset(cd.DeepCopy())

			newManager := func(nodeName, podIP string) *ComputeDomainStatusManager {
				return &ComputeDomainStatusManager{
					config: &ManagerConfig{
						clientsets: flags.ClientSets{
							Nvidia: client,
						},
						nodeName:               nodeName,
						podIP:                  podIP,
						cliqueID:               cliqueID,
						computeDomainName:      name,
						computeDomainNamespace: namespace,
						computeDomainUUID:      uid,
						maxNodesPerIMEXDomain:  8,
					},
					mutationCache: &noopMutationCache{},
				}
			}

			managers := make([]*ComputeDomainStatusManager, 0, numWriters)
			staleSnapshots := make([]*nvapi.ComputeDomain, 0, numWriters)
			// Managers read the same initial snapshot before updating the CD status.
			for i := 0; i < numWriters; i++ {
				managers = append(managers, newManager(
					fmt.Sprintf("node-%d", i),
					fmt.Sprintf("10.0.0.%d", i+1),
				))

				staleCD, err := client.ResourceV1beta1().ComputeDomains(namespace).Get(context.Background(), name, metav1.GetOptions{})
				if err != nil {
					t.Fatalf("get stale snapshot %d: %v", i, err)
				}
				staleSnapshots = append(staleSnapshots, staleCD)
			}

			for i, manager := range managers {
				if _, err := manager.syncNodeInfoToCD(context.Background(), staleSnapshots[i]); err != nil {
					t.Fatalf("sync manager %d: %v", i, err)
				}
			}

			finalCD, err := client.ResourceV1beta1().ComputeDomains(namespace).Get(context.Background(), name, metav1.GetOptions{})
			if err != nil {
				t.Fatalf("get final ComputeDomain: %v", err)
			}

			if got := len(finalCD.Status.Nodes); got != numWriters {
				t.Fatalf("expected all %d node updates to be present, got %d nodes: %#v", numWriters, got, finalCD.Status.Nodes)
			}

			nodesByName := make(map[string]*nvapi.ComputeDomainNode, len(finalCD.Status.Nodes))
			for _, node := range finalCD.Status.Nodes {
				nodesByName[node.Name] = node
			}

			for i := 0; i < numWriters; i++ {
				name := fmt.Sprintf("node-%d", i)
				if _, exists := nodesByName[name]; !exists {
					t.Fatalf("expected node %q to survive concurrent updates, got nodes: %#v", name, finalCD.Status.Nodes)
				}
			}
		})
	}
}
