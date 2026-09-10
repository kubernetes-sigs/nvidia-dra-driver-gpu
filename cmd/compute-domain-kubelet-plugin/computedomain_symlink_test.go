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
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestWriteConfigFileDoesNotFollowSymlink(t *testing.T) {
	template := filepath.Join(t.TempDir(), "template.cfg")
	require.NoError(t, os.WriteFile(template, []byte("template contents"), 0644))

	const domainID = "d3b07384-d9a7-4e2b-8f1a-2c1e6b5a9f00"

	// Plant a symlink where the plugin writes imexd.cfg.tmpl (as a compromised daemon
	// container with the read-write /imexd mount could) and assert the write does not
	// follow it out of the per-domain directory.
	check := func(t *testing.T, configRoot, victim, linkTarget string) {
		require.NoError(t, os.WriteFile(victim, []byte("original"), 0644))
		require.NoError(t, os.Symlink(linkTarget, filepath.Join(configRoot, domainID, "imexd.cfg.tmpl")))

		settings, err := (&ComputeDomainManager{configFilesRoot: configRoot}).NewSettings(domainID)
		require.NoError(t, err)
		settings.templateSourcePath = template
		require.Error(t, settings.WriteConfigFile(context.Background()))

		got, err := os.ReadFile(victim)
		require.NoError(t, err)
		require.Equal(t, "original", string(got), "write followed the symlink and clobbered %s", victim)
	}

	t.Run("target outside the config root", func(t *testing.T) {
		configRoot := t.TempDir()
		require.NoError(t, os.MkdirAll(filepath.Join(configRoot, domainID), 0755))
		victim := filepath.Join(t.TempDir(), "victim")
		check(t, configRoot, victim, victim)
	})

	t.Run("target in another domain under the config root", func(t *testing.T) {
		configRoot := t.TempDir()
		require.NoError(t, os.MkdirAll(filepath.Join(configRoot, domainID), 0755))
		other := filepath.Join(configRoot, "other-domain")
		require.NoError(t, os.MkdirAll(other, 0755))
		check(t, configRoot, filepath.Join(other, "victim"), "../other-domain/victim")
	})
}

// WriteConfigFile owns creating the per-domain directory now that Prepare no
// longer does, so the ordinary path is worth pinning too.
func TestWriteConfigFileCreatesTheDomainDirectory(t *testing.T) {
	template := filepath.Join(t.TempDir(), "template.cfg")
	require.NoError(t, os.WriteFile(template, []byte("template contents"), 0644))

	configRoot := t.TempDir()
	const domainID = "d3b07384-d9a7-4e2b-8f1a-2c1e6b5a9f00"

	// A neighbour that must come through untouched.
	const otherDomainID = "a1b2c3d4-e5f6-7890-abcd-ef1234567890"
	require.NoError(t, os.MkdirAll(filepath.Join(configRoot, otherDomainID), 0755))
	other := filepath.Join(configRoot, otherDomainID, "imexd.cfg.tmpl")
	require.NoError(t, os.WriteFile(other, []byte("someone else"), 0644))

	settings, err := (&ComputeDomainManager{configFilesRoot: configRoot}).NewSettings(domainID)
	require.NoError(t, err)
	settings.templateSourcePath = template

	// The domain directory does not exist yet.
	require.NoError(t, settings.WriteConfigFile(context.Background()))

	written := filepath.Join(configRoot, domainID, "imexd.cfg.tmpl")
	got, err := os.ReadFile(written)
	require.NoError(t, err)
	require.Equal(t, "template contents", string(got))

	info, err := os.Stat(written)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0644), info.Mode().Perm())

	untouched, err := os.ReadFile(other)
	require.NoError(t, err)
	require.Equal(t, "someone else", string(untouched))
}
