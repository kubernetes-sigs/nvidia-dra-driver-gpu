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
	"testing"

	"github.com/stretchr/testify/require"

	"sigs.k8s.io/dra-driver-nvidia-gpu/pkg/featuregates"
)

func TestLocalIPCConfigFeatureGate(t *testing.T) {
	old := featuregates.Enabled(featuregates.LocalIPCDirectory)
	t.Cleanup(func() {
		require.NoError(t, featuregates.FeatureGates().SetFromMap(map[string]bool{
			string(featuregates.LocalIPCDirectory): old,
		}))
	})

	config := &LocalIPCConfig{Enabled: true}

	require.NoError(t, featuregates.FeatureGates().SetFromMap(map[string]bool{
		string(featuregates.LocalIPCDirectory): false,
	}))
	require.Error(t, config.Validate())
	require.NoError(t, (&LocalIPCConfig{}).Validate())

	require.NoError(t, featuregates.FeatureGates().SetFromMap(map[string]bool{
		string(featuregates.LocalIPCDirectory): true,
	}))
	require.NoError(t, config.Validate())
}
