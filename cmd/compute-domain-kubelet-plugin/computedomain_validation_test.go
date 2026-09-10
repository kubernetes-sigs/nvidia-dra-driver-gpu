/*
Copyright The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNewSettingsRejectsUnsafeDomainID(t *testing.T) {
	m := &ComputeDomainManager{configFilesRoot: t.TempDir()}

	// A UID-like value is accepted and its paths stay under the config root.
	settings, err := m.NewSettings("d3b07384-d9a7-4e2b-8f1a-2c1e6b5a9f00")
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(settings.rootDir, m.configFilesRoot+string(filepath.Separator)))

	// Anything that is not a single path segment is rejected before any path is built.
	for _, bad := range []string{"", ".", "..", "../..", "../../../driver-root/tmp/evil", "/etc/cron.d/x", "a/b", "a\x00b"} {
		_, err := m.NewSettings(bad)
		require.Error(t, err, "NewSettings(%q) should be rejected", bad)
	}
}
