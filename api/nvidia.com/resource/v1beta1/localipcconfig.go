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

package v1beta1

import (
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"sigs.k8s.io/dra-driver-nvidia-gpu/pkg/featuregates"
)

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// LocalIPCConfig enables a claim-scoped directory for same-node local IPC.
// It must be specified without a requests selector because the directory is
// injected as a claim-level CDI edit.
type LocalIPCConfig struct {
	metav1.TypeMeta `json:",inline"`
	Enabled         bool `json:"enabled"`
}

// Normalize updates LocalIPCConfig with implied default values.
func (c *LocalIPCConfig) Normalize() error {
	return nil
}

// Validate ensures that LocalIPCConfig has valid settings.
func (c *LocalIPCConfig) Validate() error {
	if c.Enabled && !featuregates.Enabled(featuregates.LocalIPCDirectory) {
		return fmt.Errorf("local IPC directory is requested, but the %q feature gate is not enabled", featuregates.LocalIPCDirectory)
	}
	return nil
}
