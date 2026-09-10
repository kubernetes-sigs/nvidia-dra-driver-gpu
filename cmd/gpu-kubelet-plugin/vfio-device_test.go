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
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestCheckIommuEnabled(t *testing.T) {
	tests := map[string]struct {
		setup       func(t *testing.T, path string)
		wantEnabled bool
		wantErr     bool
	}{
		"missing iommu groups": {},
		"empty iommu groups": {
			setup: func(t *testing.T, path string) {
				require.NoError(t, os.MkdirAll(path, 0o755))
			},
		},
		"populated iommu groups": {
			setup: func(t *testing.T, path string) {
				require.NoError(t, os.MkdirAll(filepath.Join(path, "0"), 0o755))
			},
			wantEnabled: true,
		},
		"unexpected read error": {
			setup: func(t *testing.T, path string) {
				require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
				require.NoError(t, os.WriteFile(path, nil, 0o644))
			},
			wantErr: true,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			hostRoot := t.TempDir()
			iommuGroupsPath := filepath.Join(hostRoot, kernelIommuGroupPath)
			if tc.setup != nil {
				tc.setup(t, iommuGroupsPath)
			}

			enabled, err := checkIommuEnabled(hostRoot)
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.wantEnabled, enabled)
		})
	}
}

func TestGetDriver(t *testing.T) {
	t.Run("returns empty driver", func(t *testing.T) {
		pciDevicesPath := t.TempDir()
		pciAddress := "0000:00:01.0"
		devicePath := filepath.Join(pciDevicesPath, pciAddress)
		require.NoError(t, os.MkdirAll(devicePath, 0o755))

		driver, err := getDriver(pciDevicesPath, pciAddress)
		require.NoError(t, err)
		require.Empty(t, driver)
	})

	t.Run("returns valid driver", func(t *testing.T) {
		pciDevicesPath := t.TempDir()
		pciDriversPath := t.TempDir()
		pciAddress := "0000:00:01.0"
		devicePath := filepath.Join(pciDevicesPath, pciAddress)
		require.NoError(t, os.MkdirAll(devicePath, 0o755))

		require.NoError(t, os.Symlink(filepath.Join(pciDriversPath, "nvidia"), filepath.Join(devicePath, "driver")))
		driver, err := getDriver(pciDevicesPath, pciAddress)
		require.NoError(t, err)
		require.Equal(t, "nvidia", driver)
	})
}

func TestTryChangeDriverWithTimeout(t *testing.T) {
	vm := &VfioPciManager{
		nvidiaEnabled: true,
	}

	t.Run("returns success", func(t *testing.T) {
		err := vm.tryChangeDriverWithTimeout(context.Background(), time.Second, func() error {
			return nil
		})
		require.NoError(t, err)
	})

	t.Run("returns error", func(t *testing.T) {
		expected := errors.New("work function failed")
		err := vm.tryChangeDriverWithTimeout(context.Background(), time.Second, func() error {
			return expected
		})
		require.ErrorIs(t, err, expected)
	})

	t.Run("returns on timeout", func(t *testing.T) {
		started := make(chan struct{})
		release := make(chan struct{})
		finished := make(chan struct{})

		err := vm.tryChangeDriverWithTimeout(context.Background(), 10*time.Millisecond, func() error {
			close(started)
			defer close(finished)
			<-release
			return nil
		})
		require.ErrorIs(t, err, context.DeadlineExceeded)
		requireClosed(t, started)

		close(release)
		requireEventuallyClosed(t, finished)
	})

	t.Run("returns on caller cancellation", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		started := make(chan struct{})
		release := make(chan struct{})
		finished := make(chan struct{})

		go func() {
			<-started
			cancel()
		}()

		err := vm.tryChangeDriverWithTimeout(ctx, time.Second, func() error {
			close(started)
			defer close(finished)
			<-release
			return nil
		})
		require.ErrorIs(t, err, context.Canceled)

		close(release)
		requireEventuallyClosed(t, finished)
	})
}

func requireClosed(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	default:
		t.Fatal("expected channel to be closed")
	}
}

func requireEventuallyClosed(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for channel to close")
	}
}
