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
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	resourceapi "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	cdiapi "tags.cncf.io/container-device-interface/pkg/cdi"
	cdispec "tags.cncf.io/container-device-interface/specs-go"

	configapi "sigs.k8s.io/dra-driver-nvidia-gpu/api/nvidia.com/resource/v1beta1"
)

const (
	localIPCDirectoryName = "local-ipc"
	localIPCContainerPath = "/run/nvidia-dra/local-ipc"
)

type LocalIPCInfo struct {
	HostPath      string
	ContainerPath string
}

type LocalIPCManager struct {
	root string
}

// GetCDIContainerEdits returns the CDI edits for a prepared local IPC directory.
func (i *LocalIPCInfo) GetCDIContainerEdits() *cdiapi.ContainerEdits {
	return &cdiapi.ContainerEdits{
		ContainerEdits: &cdispec.ContainerEdits{
			Env: []string{"NVIDIA_DRA_LOCAL_IPC_DIR=" + i.ContainerPath},
			Mounts: []*cdispec.Mount{{
				ContainerPath: i.ContainerPath,
				HostPath:      i.HostPath,
				Options:       []string{"rw", "nosuid", "nodev", "noexec", "bind"},
			}},
		},
	}
}

func NewLocalIPCManager(driverPluginPath string) *LocalIPCManager {
	return &LocalIPCManager{root: filepath.Join(driverPluginPath, localIPCDirectoryName)}
}

func getLocalIPCConfig(allocation *resourceapi.AllocationResult) (*configapi.LocalIPCConfig, error) {
	result, err := decodeLocalIPCConfig(configapi.StrictDecoder, allocation)
	if err != nil || result == nil {
		return result, err
	}
	if err := result.Validate(); err != nil {
		return nil, err
	}
	return result, nil
}

func getCheckpointLocalIPCConfig(allocation *resourceapi.AllocationResult) (*configapi.LocalIPCConfig, error) {
	return decodeLocalIPCConfig(configapi.NonstrictDecoder, allocation)
}

func decodeLocalIPCConfig(decoder runtime.Decoder, allocation *resourceapi.AllocationResult) (*configapi.LocalIPCConfig, error) {
	if allocation == nil {
		return nil, nil
	}

	var localIPCConfigs []resourceapi.DeviceAllocationConfiguration
	for _, config := range allocation.Devices.Config {
		if config.Opaque == nil || config.Opaque.Driver != DriverName {
			continue
		}

		// Inspect TypeMeta before runtime decoding so checkpoint recovery can
		// ignore unrelated config kinds added by newer driver versions.
		var typeMeta metav1.TypeMeta
		if err := json.Unmarshal(config.Opaque.Parameters.Raw, &typeMeta); err != nil {
			return nil, fmt.Errorf("decode config type metadata: %w", err)
		}
		if typeMeta.APIVersion != configapi.GroupName+"/"+configapi.Version || typeMeta.Kind != configapi.LocalIPCConfigKind {
			continue
		}
		localIPCConfigs = append(localIPCConfigs, config)
	}

	configs, err := GetOpaqueDeviceConfigs(decoder, DriverName, localIPCConfigs)
	if err != nil {
		return nil, err
	}

	var result *configapi.LocalIPCConfig
	for _, config := range configs {
		localIPC, ok := config.Config.(*configapi.LocalIPCConfig)
		if !ok {
			return nil, fmt.Errorf("decoded local IPC config has unexpected type %T", config.Config)
		}
		if len(config.Requests) != 0 {
			return nil, fmt.Errorf("local IPC config must not select individual requests")
		}
		result = localIPC.DeepCopy()
	}
	if result != nil {
		if err := result.Normalize(); err != nil {
			return nil, err
		}
	}
	return result, nil
}

func (m *LocalIPCManager) Prepare(claimUID string, allocation *resourceapi.AllocationResult) (*LocalIPCInfo, error) {
	config, err := getLocalIPCConfig(allocation)
	if err != nil {
		return nil, err
	}
	if config == nil || !config.Enabled {
		return nil, nil
	}
	return m.ensureDirectory(claimUID)
}

func (m *LocalIPCManager) Restore(claimUID string, allocation *resourceapi.AllocationResult) error {
	config, err := getCheckpointLocalIPCConfig(allocation)
	if err != nil {
		return err
	}
	if config == nil || !config.Enabled {
		return nil
	}
	_, err = m.ensureDirectory(claimUID)
	return err
}

func (m *LocalIPCManager) ensureDirectory(claimUID string) (*LocalIPCInfo, error) {
	path, err := m.claimPath(claimUID)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(m.root, 0o750); err != nil {
		return nil, fmt.Errorf("create local IPC root: %w", err)
	}
	if _, err := m.validateRoot(); err != nil {
		return nil, err
	}
	if err := os.Mkdir(path, os.ModeSticky|0o777); err != nil && !os.IsExist(err) {
		return nil, fmt.Errorf("create claim local IPC directory: %w", err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("stat claim local IPC directory: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return nil, fmt.Errorf("claim local IPC path is not a directory: %q", path)
	}
	if err := os.Chmod(path, os.ModeSticky|0o777); err != nil {
		return nil, fmt.Errorf("set claim local IPC directory permissions: %w", err)
	}
	return &LocalIPCInfo{HostPath: path, ContainerPath: localIPCContainerPath}, nil
}

func (m *LocalIPCManager) Remove(claimUID string) error {
	path, err := m.claimPath(claimUID)
	if err != nil {
		return err
	}
	exists, err := m.validateRoot()
	if err != nil {
		return err
	}
	if !exists {
		return nil
	}
	if err := os.RemoveAll(path); err != nil {
		return fmt.Errorf("remove claim local IPC directory: %w", err)
	}
	return nil
}

func (m *LocalIPCManager) Reconcile(claims PreparedClaimsByUID) error {
	expected := make(map[string]struct{})
	for uid, claim := range claims {
		if claim.CheckpointState != ClaimCheckpointStatePrepareCompleted {
			continue
		}
		config, err := getCheckpointLocalIPCConfig(claim.Status.Allocation)
		if err != nil {
			return fmt.Errorf("decode local IPC config for checkpointed claim %q: %w", uid, err)
		}
		if config == nil || !config.Enabled {
			continue
		}
		if _, err := m.ensureDirectory(uid); err != nil {
			return err
		}
		expected[uid] = struct{}{}
	}

	exists, err := m.validateRoot()
	if err != nil {
		return err
	}
	if !exists {
		return nil
	}

	entries, err := os.ReadDir(m.root)
	if err != nil {
		return fmt.Errorf("read local IPC root: %w", err)
	}
	for _, entry := range entries {
		if _, ok := expected[entry.Name()]; ok {
			continue
		}
		if err := os.RemoveAll(filepath.Join(m.root, entry.Name())); err != nil {
			return fmt.Errorf("remove stale local IPC entry %q: %w", entry.Name(), err)
		}
	}
	return nil
}

func (m *LocalIPCManager) validateRoot() (bool, error) {
	info, err := os.Lstat(m.root)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("stat local IPC root: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return false, fmt.Errorf("local IPC root is not a directory: %q", m.root)
	}
	return true, nil
}

func (m *LocalIPCManager) claimPath(claimUID string) (string, error) {
	if claimUID == "" || claimUID == "." || claimUID == ".." || filepath.Base(claimUID) != claimUID {
		return "", fmt.Errorf("invalid claim UID %q", claimUID)
	}
	path := filepath.Join(m.root, claimUID)
	rel, err := filepath.Rel(m.root, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", fmt.Errorf("claim UID %q escapes local IPC root", claimUID)
	}
	return path, nil
}
