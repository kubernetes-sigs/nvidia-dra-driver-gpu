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
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"

	cdiapi "tags.cncf.io/container-device-interface/pkg/cdi"
	cdispec "tags.cncf.io/container-device-interface/specs-go"

	nvapi "sigs.k8s.io/dra-driver-nvidia-gpu/api/nvidia.com/resource/v1beta1"
	"sigs.k8s.io/dra-driver-nvidia-gpu/internal/common"
	"sigs.k8s.io/dra-driver-nvidia-gpu/pkg/featuregates"
	nvinformers "sigs.k8s.io/dra-driver-nvidia-gpu/pkg/nvidia.com/informers/externalversions"
)

const (
	computeDomainLabelKey = "resource.nvidia.com/computeDomain"

	// gpuCliqueLabelKey sets the node label historically set by
	// gpu-feature-discovery (see
	// https://github.com/NVIDIA/k8s-device-plugin/blob/main/docs/gpu-feature-discovery/README.md#generated-labels).
	// gpu-feature-discovery that is bundled with the k8s-device-plugin is being deprecated,
	// so the kubelet plugin now owns setting it on systems where the DRA driver is enabled.
	gpuCliqueLabelKey = "nvidia.com/gpu.clique"

	gpuCliqueLabelRefreshInterval = 10 * time.Minute

	informerResyncPeriod = 10 * time.Minute
	cleanupInterval      = 10 * time.Minute

	ComputeDomainDaemonConfigFilesDirName = "domains"
	ComputeDomainDaemonConfigTemplatePath = "/templates/compute-domain-daemon-config.tmpl.cfg"
)

type ComputeDomainManager struct {
	config        *Config
	waitGroup     sync.WaitGroup
	cancelContext context.CancelFunc

	factory  nvinformers.SharedInformerFactory
	informer cache.SharedIndexInformer

	configFilesRoot string

	cliqueIDMu sync.RWMutex
	cliqueID   string

	getCliqueIDFunc func() (string, error)
}

type ComputeDomainDaemonSettings struct {
	manager         *ComputeDomainManager
	domainID        string
	rootDir         string
	configTmplPath  string
	nodesConfigPath string

	// templateSourcePath is where the daemon config template is read from. It is
	// only ever the path below outside of tests.
	templateSourcePath string
}

func NewComputeDomainManager(config *Config, getCliqueIDFunc func() (string, error)) (*ComputeDomainManager, error) {
	factory := nvinformers.NewSharedInformerFactory(config.clientsets.Nvidia, informerResyncPeriod)
	informer := factory.Resource().V1beta1().ComputeDomains().Informer()
	configFilesRoot := filepath.Join(config.DriverPluginPath(), ComputeDomainDaemonConfigFilesDirName)

	cliqueID, err := getCliqueIDFunc()
	if err != nil {
		return nil, fmt.Errorf("error getting cliqueID: %w", err)
	}

	m := &ComputeDomainManager{
		config:          config,
		factory:         factory,
		informer:        informer,
		configFilesRoot: configFilesRoot,
		cliqueID:        cliqueID,
		getCliqueIDFunc: getCliqueIDFunc,
	}

	return m, nil
}

// CliqueID returns the most recently known GPU clique ID. Safe for
// concurrent use.
func (m *ComputeDomainManager) CliqueID() string {
	m.cliqueIDMu.RLock()
	defer m.cliqueIDMu.RUnlock()
	return m.cliqueID
}

// setCliqueID stores a newly observed GPU clique ID.
func (m *ComputeDomainManager) setCliqueID(cliqueID string) {
	m.cliqueIDMu.Lock()
	defer m.cliqueIDMu.Unlock()
	m.cliqueID = cliqueID
}

func (m *ComputeDomainManager) Start(ctx context.Context) (rerr error) {
	ctx, cancel := context.WithCancel(ctx)
	m.cancelContext = cancel

	defer func() {
		if rerr != nil {
			if err := m.Stop(); err != nil {
				klog.Errorf("error stopping ComputeDomainManager: %v", err)
			}
		}
	}()

	err := m.informer.AddIndexers(cache.Indexers{
		"computeDomainUID": uidIndexer[*nvapi.ComputeDomain],
	})
	if err != nil {
		return fmt.Errorf("error adding indexer for UIDs: %w", err)
	}

	m.waitGroup.Add(1)
	go func() {
		defer m.waitGroup.Done()
		m.factory.Start(ctx.Done())
	}()

	m.waitGroup.Add(1)
	go func() {
		defer m.waitGroup.Done()
		m.periodicCleanup(ctx)
	}()

	if !cache.WaitForCacheSync(ctx.Done(), m.informer.HasSynced) {
		return fmt.Errorf("informer cache sync for ComputeDomains failed")
	}

	if m.config.flags.gpuCliqueLabelEnabled {
		if err := m.SetGPUCliqueLabel(ctx); err != nil {
			return fmt.Errorf("error setting %s node label: %w", gpuCliqueLabelKey, err)
		}
	}

	if m.getCliqueIDFunc != nil {
		m.waitGroup.Add(1)
		go func() {
			defer m.waitGroup.Done()
			m.periodicGPUCliqueIDRefresh(ctx)
		}()
	}

	return nil
}

//nolint:contextcheck
func (m *ComputeDomainManager) Stop() error {
	if m.cancelContext != nil {
		m.cancelContext()
	}
	m.waitGroup.Wait()

	if m.config.flags.gpuCliqueLabelEnabled {
		rmCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := m.RemoveGPUCliqueLabel(rmCtx); err != nil {
			klog.Errorf("error removing %s node label: %v", gpuCliqueLabelKey, err)
		}
	}

	return nil
}

func (m *ComputeDomainManager) NewSettings(domainID string) (*ComputeDomainDaemonSettings, error) {
	if err := nvapi.ValidateDomainID(domainID); err != nil {
		return nil, fmt.Errorf("invalid domainID: %w", err)
	}
	return &ComputeDomainDaemonSettings{
		manager:            m,
		domainID:           domainID,
		rootDir:            filepath.Join(m.configFilesRoot, domainID),
		configTmplPath:     filepath.Join(m.configFilesRoot, domainID, "imexd.cfg.tmpl"),
		nodesConfigPath:    filepath.Join(m.configFilesRoot, domainID, "nodes.cfg"),
		templateSourcePath: ComputeDomainDaemonConfigTemplatePath,
	}, nil
}

func (m *ComputeDomainManager) GetComputeDomainChannelContainerEdits(devRoot string, info *common.NVcapDeviceInfo) *cdiapi.ContainerEdits {
	return &cdiapi.ContainerEdits{
		ContainerEdits: &cdispec.ContainerEdits{
			DeviceNodes: []*cdispec.DeviceNode{info.CDICharDevNode()},
		},
	}
}

// GetCDIContainerEditsCommon() returns the CDI spec edits always required for
// launching the CD Daemon (whether or not it tries to launch an IMEX daemon
// internally).
func (s *ComputeDomainDaemonSettings) GetCDIContainerEditsCommon(ctx context.Context) (*cdiapi.ContainerEdits, error) {
	cd, err := s.manager.GetComputeDomain(ctx, s.domainID)
	if err != nil {
		return nil, fmt.Errorf("error getting compute domain %s: %w", s.domainID, err)
	}
	if cd == nil {
		return nil, fmt.Errorf("compute domain not found: %s", s.domainID)
	}

	edits := &cdiapi.ContainerEdits{
		ContainerEdits: &cdispec.ContainerEdits{
			Env: []string{
				fmt.Sprintf("CLIQUE_ID=%s", s.manager.CliqueID()),
				fmt.Sprintf("COMPUTE_DOMAIN_UUID=%s", cd.UID),
				fmt.Sprintf("COMPUTE_DOMAIN_NAME=%s", cd.Name),
				fmt.Sprintf("COMPUTE_DOMAIN_NAMESPACE=%s", cd.Namespace),
			},
			Mounts: []*cdispec.Mount{
				{
					// imexDaemonConfigDirPath   = "/imexd"
					ContainerPath: "/imexd",
					HostPath:      s.rootDir,
					Options:       []string{"rw", "nosuid", "nodev", "bind"},
				},
			},
		},
	}
	return edits, nil
}

func (s *ComputeDomainDaemonSettings) GetDomainID() string {
	return s.domainID
}

// GetCDIContainerEditsForImex() returns the CDI spec edits only required for
// launching the CD daemon when it actually wraps an IMEX daemon.
func (s *ComputeDomainDaemonSettings) GetCDIContainerEditsForImex(ctx context.Context, devRoot string, info *common.NVcapDeviceInfo) *cdiapi.ContainerEdits {
	edits := &cdiapi.ContainerEdits{
		ContainerEdits: &cdispec.ContainerEdits{
			DeviceNodes: []*cdispec.DeviceNode{info.CDICharDevNode()},
		},
	}
	return edits
}

func (s *ComputeDomainDaemonSettings) Prepare(ctx context.Context) error {
	if err := s.WriteConfigFile(ctx); err != nil {
		return fmt.Errorf("error writing config file %v: %w", s.configTmplPath, err)
	}

	return nil
}

func (s *ComputeDomainDaemonSettings) Unprepare(ctx context.Context) error {
	// TODO: Only actually remove this directory once the ComputeDomain has
	// been deleted. There is a (rare) chance when a pod gets force deleted
	// and a new pod associated with the same compute domain gets started
	// that deleting this here will occur *after* the creation from the new
	// pod, rendering this directory invalid when trying to be used by the
	// new pod. For now, just wait for the cleanup loop to take care of
	// this, but in the future let's do this cleanup on ComputeDomain
	// deletion (in addition to the cleanup loop).
	// err := os.RemoveAll(s.rootDir); err != nil {
	//	return fmt.Errorf("error removing directory %v: %w", s.rootDir, err)
	//}
	return nil
}

func (s *ComputeDomainDaemonSettings) WriteConfigFile(ctx context.Context) error {
	configBytes, err := os.ReadFile(s.templateSourcePath)
	if err != nil {
		return fmt.Errorf("error reading template file: %w", err)
	}

	// The per-domain directory is bind-mounted read-write into the daemon container, so write
	// through an os.Root scoped to that directory: a symlink there can't redirect the write to
	// another domain's directory or outside the config root. This assumes the config root and
	// the per-domain directory entry itself stay ours; the daemon is given the contents of its
	// own directory, not the entry in the parent. os.Root is not a filesystem sandbox and says
	// nothing about mounts already in place underneath it.
	if err := os.MkdirAll(s.manager.configFilesRoot, 0755); err != nil {
		return fmt.Errorf("error creating config root %v: %w", s.manager.configFilesRoot, err)
	}
	configRoot, err := os.OpenRoot(s.manager.configFilesRoot)
	if err != nil {
		return fmt.Errorf("error opening config root %v: %w", s.manager.configFilesRoot, err)
	}
	defer configRoot.Close()

	if err := configRoot.MkdirAll(s.domainID, 0755); err != nil {
		return fmt.Errorf("error creating ComputeDomain daemon directory: %w", err)
	}
	domainRoot, err := configRoot.OpenRoot(s.domainID)
	if err != nil {
		return fmt.Errorf("error opening ComputeDomain daemon directory: %w", err)
	}
	defer domainRoot.Close()

	if err := domainRoot.WriteFile("imexd.cfg.tmpl", configBytes, 0644); err != nil {
		return fmt.Errorf("error writing config file %v: %w", s.configTmplPath, err)
	}

	return nil
}

func (m *ComputeDomainManager) AssertComputeDomainReady(ctx context.Context, cdUID string) error {
	cd, err := m.GetComputeDomain(ctx, cdUID)
	if err != nil {
		return fmt.Errorf("error getting ComputeDomain: %w", err)
	}
	if cd == nil {
		return fmt.Errorf("ComputeDomain not found: %s", cdUID)
	}

	// Check if the current node is ready in the ComputeDomain
	if !m.isCurrentNodeReady(ctx, cd) {
		return fmt.Errorf("current node not ready in ComputeDomain")
	}

	return nil
}

// isCurrentNodeReady checks if the current node is marked as ready in the ComputeDomain.
// When the feature gate is enabled, we check both the clique and the status to ensure
// that compute domains started before the feature gate was enabled continue to work
// even after the feature gate is enabled.
func (m *ComputeDomainManager) isCurrentNodeReady(ctx context.Context, cd *nvapi.ComputeDomain) bool {
	if featuregates.Enabled(featuregates.ComputeDomainCliques) {
		if m.isCurrentNodeReadyInClique(ctx, cd) {
			return true
		}
	}
	return m.isCurrentNodeReadyInStatus(cd)
}

// isCurrentNodeReadyInStatus checks if the current node is marked as ready in the ComputeDomain status.
func (m *ComputeDomainManager) isCurrentNodeReadyInStatus(cd *nvapi.ComputeDomain) bool {
	for _, node := range cd.Status.Nodes {
		if node.Name == m.config.flags.nodeName {
			return node.Status == nvapi.ComputeDomainStatusReady
		}
	}
	return false
}

// isCurrentNodeReadyInClique checks if the current node is marked as ready in the ComputeDomainClique.
func (m *ComputeDomainManager) isCurrentNodeReadyInClique(ctx context.Context, cd *nvapi.ComputeDomain) bool {
	cliqueName := fmt.Sprintf("%s.%s", cd.UID, m.CliqueID())

	clique, err := m.config.clientsets.Nvidia.ResourceV1beta1().ComputeDomainCliques(m.config.flags.namespace).Get(ctx, cliqueName, metav1.GetOptions{})
	if err != nil {
		klog.Errorf("error getting ComputeDomainClique %s: %v", cliqueName, err)
		return false
	}

	for _, daemon := range clique.Daemons {
		if daemon.NodeName == m.config.flags.nodeName {
			return daemon.Status == nvapi.ComputeDomainStatusReady
		}
	}
	return false
}

func (m *ComputeDomainManager) AssertComputeDomainNamespace(ctx context.Context, claimNamespace, cdUID string) error {
	cd, err := m.GetComputeDomain(ctx, cdUID)
	if err != nil {
		return fmt.Errorf("error getting ComputeDomain: %w", err)
	}
	if cd == nil {
		return fmt.Errorf("ComputeDomain not found: %s", cdUID)
	}

	if cd.Namespace != claimNamespace {
		return fmt.Errorf("the ResourceClaim's namespace is different than the ComputeDomain's namespace")
	}

	return nil
}

func (m *ComputeDomainManager) AddNodeLabel(ctx context.Context, cdUID string) error {
	node, err := m.config.clientsets.Core.CoreV1().Nodes().Get(ctx, m.config.flags.nodeName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("error retrieving Node: %w", err)
	}

	currentValue, exists := node.Labels[computeDomainLabelKey]
	if exists && currentValue != cdUID {
		return fmt.Errorf("label already exists for a different ComputeDomain")
	}

	if exists && currentValue == cdUID {
		return nil
	}

	newNode := node.DeepCopy()
	if newNode.Labels == nil {
		newNode.Labels = make(map[string]string)
	}
	newNode.Labels[computeDomainLabelKey] = cdUID

	if _, err = m.config.clientsets.Core.CoreV1().Nodes().Update(ctx, newNode, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("error updating Node with label: %w", err)
	}

	return nil
}

// RemoveNodeLabel() attempts removal and returns no error if the label was
// removed or didn't exist in the first place.
func (m *ComputeDomainManager) RemoveNodeLabel(ctx context.Context, cdUID string) error {
	node, err := m.config.clientsets.Core.CoreV1().Nodes().Get(ctx, m.config.flags.nodeName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("error retrieving Node: %w", err)
	}

	if _, exists := node.Labels[computeDomainLabelKey]; !exists {
		return nil
	}

	if node.Labels[computeDomainLabelKey] != cdUID {
		return nil
	}

	newNode := node.DeepCopy()
	delete(newNode.Labels, computeDomainLabelKey)

	if _, err := m.config.clientsets.Core.CoreV1().Nodes().Update(ctx, newNode, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("error updating Node to remove label: %w", err)
	}

	return nil
}

// SetGPUCliqueLabel sets the nvidia.com/gpu.clique node label based on the
// clique ID discovered from NVML at plugin startup. If no clique ID was
// discovered (e.g. fabric not attached), the label is simply left unset.
func (m *ComputeDomainManager) SetGPUCliqueLabel(ctx context.Context) error {
	cliqueID := m.CliqueID()
	if cliqueID == "" {
		return nil
	}

	patch := map[string]any{
		"metadata": map[string]any{
			"labels": map[string]string{
				gpuCliqueLabelKey: cliqueID,
			},
		},
	}

	patchBytes, err := json.Marshal(patch)
	if err != nil {
		return fmt.Errorf("failed to marshal patch: %w", err)
	}

	if _, err := m.config.clientsets.Core.CoreV1().Nodes().Patch(ctx, m.config.flags.nodeName, types.MergePatchType, patchBytes, metav1.PatchOptions{}); err != nil {
		return fmt.Errorf("error patching node with label %s: %w", gpuCliqueLabelKey, err)
	}

	return nil
}

// RemoveGPUCliqueLabel removes the nvidia.com/gpu.clique node label, e.g. on
// plugin shutdown.
func (m *ComputeDomainManager) RemoveGPUCliqueLabel(ctx context.Context) error {
	patch := map[string]any{
		"metadata": map[string]any{
			"labels": map[string]any{
				gpuCliqueLabelKey: nil,
			},
		},
	}

	patchBytes, err := json.Marshal(patch)
	if err != nil {
		return fmt.Errorf("failed to marshal patch: %w", err)
	}

	if _, err := m.config.clientsets.Core.CoreV1().Nodes().Patch(ctx, m.config.flags.nodeName, types.MergePatchType, patchBytes, metav1.PatchOptions{}); err != nil {
		return fmt.Errorf("error removing Node label %s: %w", gpuCliqueLabelKey, err)
	}

	return nil
}

// periodicGPUCliqueIDRefresh periodically re-reads the GPU clique ID from
// NVML and updates the nvidia.com/gpu.clique node label if it changed.
func (m *ComputeDomainManager) periodicGPUCliqueIDRefresh(ctx context.Context) {
	ticker := time.NewTicker(gpuCliqueLabelRefreshInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			if err := m.refreshGPUCliqueID(ctx); err != nil {
				klog.Errorf("error refreshing %s node label: %v", gpuCliqueLabelKey, err)
			}
		case <-ctx.Done():
			return
		}
	}
}

// refreshGPUCliqueLabel re-reads the GPU clique ID and, if it differs from
// the last known value, updates the in-memory state and the node label.
func (m *ComputeDomainManager) refreshGPUCliqueID(ctx context.Context) error {
	newCliqueID, err := m.getCliqueIDFunc()
	if err != nil {
		return fmt.Errorf("error getting cliqueID: %w", err)
	}

	if oldCliqueID := m.CliqueID(); newCliqueID != "" && newCliqueID != oldCliqueID {
		klog.Infof("GPU clique ID changed from %q to %q, updating %s node label", oldCliqueID, newCliqueID, gpuCliqueLabelKey)
		m.setCliqueID(newCliqueID)
	}

	if m.config.flags.gpuCliqueLabelEnabled {
		if err := m.SetGPUCliqueLabel(ctx); err != nil {
			return fmt.Errorf("error updating %s node label: %w", gpuCliqueLabelKey, err)
		}
	}

	return nil
}

func (m *ComputeDomainManager) GetComputeDomain(ctx context.Context, cdUID string) (*nvapi.ComputeDomain, error) {
	cds, err := m.informer.GetIndexer().ByIndex("computeDomainUID", cdUID)
	if err != nil {
		return nil, fmt.Errorf("error retrieving ComputeDomain by UID: %w", err)
	}
	if len(cds) == 0 {
		return nil, nil
	}
	if len(cds) != 1 {
		return nil, fmt.Errorf("multiple ComputeDomains with the same UID")
	}
	cd, ok := cds[0].(*nvapi.ComputeDomain)
	if !ok {
		return nil, fmt.Errorf("failed to cast to ComputeDomain")
	}
	return cd, nil
}

func (m *ComputeDomainManager) periodicCleanup(ctx context.Context) {
	ticker := time.NewTicker(cleanupInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			klog.V(6).Infof("Running periodic cleanup to remove stale ComputeDomain artifacts")

			_, err := os.Stat(m.configFilesRoot)
			if os.IsNotExist(err) {
				continue
			}
			if err != nil {
				klog.Errorf("error checking for existence of directory '%s': %v", m.configFilesRoot, err)
				continue
			}

			entries, err := os.ReadDir(m.configFilesRoot)
			if err != nil {
				klog.Errorf("error reading entries under directory '%s': %v", m.configFilesRoot, err)
				continue
			}

			for _, e := range entries {
				if !e.IsDir() {
					continue
				}

				// Convention: per-CD directory with CD UID as basename
				uid := e.Name()
				path := filepath.Join(m.configFilesRoot, e.Name())

				computeDomain, err := m.GetComputeDomain(ctx, uid)
				if err != nil {
					klog.Errorf("error getting ComputeDomain: %v", err)
					continue
				}

				// CD still exists, do not clean up
				if computeDomain != nil {
					continue
				}

				klog.V(6).Infof("Stale directory found for ComputeDomain '%s', running cleanup", uid)

				if err := os.RemoveAll(path); err != nil {
					klog.Errorf("error removing artifacts directory for ComputeDomain '%s': %v", uid, err)
					continue
				}
			}
		case <-ctx.Done():
			return
		}
	}
}
