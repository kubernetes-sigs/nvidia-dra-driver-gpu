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

package featuregates

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	utilversion "k8s.io/apimachinery/pkg/util/version"
	"k8s.io/component-base/featuregate"
	logsapi "k8s.io/component-base/logs/api/v1"
)

// TestVersion is a high version used in tests to ensure all feature gates are visible
// regardless of the actual project version being tested.
var TestVersion = utilversion.MajorMinor(999, 999)

func TestNewFeatureGatesWithKubeEmulationDoesNotPanic(t *testing.T) {
	require.NotPanics(t, func() {
		_ = newFeatureGates(featureGateEmulationVersion)
	})
	fg := newFeatureGates(featureGateEmulationVersion)
	require.True(t, fg.Enabled(logsapi.ContextualLogging))
}

// Test feature gates covering all lifecycle stages
// These are completely fake features used only for testing - no real feature gate values are used.
const (
	// Alpha features (disabled by default, changeable).
	TestAlphaFeature1 featuregate.Feature = "TestAlphaFeature1"
	TestAlphaFeature2 featuregate.Feature = "TestAlphaFeature2"

	// Beta features (enabled by default, changeable).
	TestBetaFeature1 featuregate.Feature = "TestBetaFeature1"
	TestBetaFeature2 featuregate.Feature = "TestBetaFeature2"

	// GA features (enabled by default, locked to default).
	TestGAFeature1 featuregate.Feature = "TestGAFeature1"
	TestGAFeature2 featuregate.Feature = "TestGAFeature2"

	// Deprecated features (disabled by default, locked to default).
	TestDeprecatedFeature1 featuregate.Feature = "TestDeprecatedFeature1"
	TestDeprecatedFeature2 featuregate.Feature = "TestDeprecatedFeature2"
)

// createTestFeatureGates creates fake feature gates covering all lifecycle stages.
func createTestFeatureGates() map[featuregate.Feature]featuregate.FeatureSpec {
	return map[featuregate.Feature]featuregate.FeatureSpec{
		// Alpha features - disabled by default, changeable
		TestAlphaFeature1: {
			Default:    false,
			PreRelease: featuregate.Alpha,
		},
		TestAlphaFeature2: {
			Default:    false,
			PreRelease: featuregate.Alpha,
		},

		// Beta features - enabled by default, changeable
		TestBetaFeature1: {
			Default:    true,
			PreRelease: featuregate.Beta,
		},
		TestBetaFeature2: {
			Default:    true,
			PreRelease: featuregate.Beta,
		},

		// GA features - enabled by default, locked to default (immutable)
		TestGAFeature1: {
			Default:       true,
			LockToDefault: true,
			PreRelease:    featuregate.GA,
		},
		TestGAFeature2: {
			Default:       true,
			LockToDefault: true,
			PreRelease:    featuregate.GA,
		},

		// Deprecated features - disabled by default, locked to default (immutable)
		TestDeprecatedFeature1: {
			Default:       false,
			LockToDefault: true,
			PreRelease:    featuregate.Deprecated,
		},
		TestDeprecatedFeature2: {
			Default:       false,
			LockToDefault: true,
			PreRelease:    featuregate.Deprecated,
		},
	}
}

// =============================================================================
// Basic Functionality Tests
// =============================================================================

func TestDefaultFeatureGates(t *testing.T) {
	t.Run("DefaultFeatureGatesMap", func(t *testing.T) {
		// Test that defaultFeatureGates contains some feature gates
		require.NotEmpty(t, defaultFeatureGates, "defaultFeatureGates should contain at least one feature gate")

		// Test that all features in defaultFeatureGates have valid specs
		for feature, specs := range defaultFeatureGates {
			require.NotEmpty(t, string(feature), "Feature name should not be empty")
			require.NotEmpty(t, specs, "Feature %s should have at least one spec", feature)

			// Check that each spec has valid PreRelease stage
			for i, spec := range specs {
				require.NotEmpty(t, spec.PreRelease, "Feature %s spec[%d] should have a valid PreRelease stage", feature, i)
			}
		}
	})

	t.Run("DefaultFeatureGatesFunction", func(t *testing.T) {
		// Test the actual newFeatureGates function
		fg := newFeatureGates(TestVersion)

		require.NotNil(t, fg, "DefaultFeatureGates() should not return nil")

		// Test that it's a valid feature gate (basic functionality test)
		knownFeatures := fg.KnownFeatures()
		require.NotEmpty(t, knownFeatures, "DefaultFeatureGates should have some known features")

		// Test that the system is functional by querying a real feature
		require.NotPanics(t, func() {
			_ = fg.Enabled(TimeSlicingSettings)
		}, "Should be able to query real features")

		// Test that real features have expected defaults
		require.False(t, fg.Enabled(TimeSlicingSettings), "TimeSlicingSettings should be disabled by default (alpha)")
		require.False(t, fg.Enabled(HostManagedIMEXDaemon), "HostManagedIMEXDaemon should be disabled by default (alpha)")
		require.False(t, fg.Enabled(ResourceClaimDeviceStatus), "ResourceClaimDeviceStatus should be disabled by default (alpha)")
	})
}

func TestEnabledConvenienceFunction(t *testing.T) {
	// Test the convenience function with test features
	fg := featuregate.NewFeatureGate()
	testGates := createTestFeatureGates()
	err := fg.Add(testGates)
	require.NoError(t, err, "fg.Add(testGates) should not fail")

	// Test with alpha feature (should be disabled by default)
	require.False(t, fg.Enabled(TestAlphaFeature1), "Alpha feature should be disabled by default")

	// Enable a test feature and test
	err = fg.SetFromMap(map[string]bool{
		string(TestAlphaFeature1): true,
	})
	require.NoError(t, err, "SetFromMap should not fail")

	require.True(t, fg.Enabled(TestAlphaFeature1), "Alpha feature should be enabled after SetFromMap")
}

// =============================================================================
// Lifecycle Management Tests
// =============================================================================

func TestFeatureGateLifecycle(t *testing.T) {
	// Test all feature gate lifecycle stages with comprehensive scenarios
	fg := featuregate.NewFeatureGate()
	testGates := createTestFeatureGates()
	err := fg.Add(testGates)
	require.NoError(t, err, "fg.Add should not fail")

	t.Run("AlphaFeatures", func(t *testing.T) {
		alphaFeatures := []featuregate.Feature{TestAlphaFeature1, TestAlphaFeature2}

		// Alpha features should be disabled by default
		for _, feature := range alphaFeatures {
			require.False(t, fg.Enabled(feature), "Alpha feature %s should be disabled by default", feature)
		}

		// Alpha features should be changeable
		err := fg.SetFromMap(map[string]bool{
			string(TestAlphaFeature1): true,
		})
		require.NoError(t, err, "Should be able to enable alpha features")

		require.True(t, fg.Enabled(TestAlphaFeature1), "Alpha feature should be enabled after explicit enabling")
		require.False(t, fg.Enabled(TestAlphaFeature2), "Non-enabled alpha feature should remain disabled")

		// Should be able to disable again
		err = fg.SetFromMap(map[string]bool{
			string(TestAlphaFeature1): false,
		})
		require.NoError(t, err, "Should be able to disable alpha features")
		require.False(t, fg.Enabled(TestAlphaFeature1), "Alpha feature should be disabled after explicit disabling")
	})

	t.Run("BetaFeatures", func(t *testing.T) {
		betaFeatures := []featuregate.Feature{TestBetaFeature1, TestBetaFeature2}

		// Beta features should be enabled by default
		for _, feature := range betaFeatures {
			require.True(t, fg.Enabled(feature), "Beta feature %s should be enabled by default", feature)
		}

		// Beta features should be changeable
		err := fg.SetFromMap(map[string]bool{
			string(TestBetaFeature1): false,
		})
		require.NoError(t, err, "Should be able to disable beta features")

		require.False(t, fg.Enabled(TestBetaFeature1), "Beta feature should be disabled after explicit disabling")
		require.True(t, fg.Enabled(TestBetaFeature2), "Non-disabled beta feature should remain enabled")

		// Should be able to enable again
		err = fg.SetFromMap(map[string]bool{
			string(TestBetaFeature1): true,
		})
		require.NoError(t, err, "Should be able to re-enable beta features")
		require.True(t, fg.Enabled(TestBetaFeature1), "Beta feature should be enabled after re-enabling")
	})

	t.Run("GAFeatures", func(t *testing.T) {
		gaFeatures := []featuregate.Feature{TestGAFeature1, TestGAFeature2}

		// GA features should be enabled by default
		for _, feature := range gaFeatures {
			require.True(t, fg.Enabled(feature), "GA feature %s should be enabled by default", feature)
		}

		// GA features should NOT be changeable (locked to default)
		err := fg.SetFromMap(map[string]bool{
			string(TestGAFeature1): false,
		})
		require.Error(t, err, "Should not be able to disable GA features (locked to default)")

		// GA features should still be enabled despite attempt to disable
		require.True(t, fg.Enabled(TestGAFeature1), "GA feature should remain enabled (locked to default)")
		require.True(t, fg.Enabled(TestGAFeature2), "GA feature should remain enabled (locked to default)")
	})

	t.Run("DeprecatedFeatures", func(t *testing.T) {
		deprecatedFeatures := []featuregate.Feature{TestDeprecatedFeature1, TestDeprecatedFeature2}

		// Deprecated features should be disabled by default
		for _, feature := range deprecatedFeatures {
			require.False(t, fg.Enabled(feature), "Deprecated feature %s should be disabled by default", feature)
		}

		// Deprecated features should NOT be changeable (locked to default)
		err := fg.SetFromMap(map[string]bool{
			string(TestDeprecatedFeature1): true,
		})
		require.Error(t, err, "Should not be able to enable deprecated features (locked to default)")

		// Deprecated features should still be disabled despite attempt to enable
		require.False(t, fg.Enabled(TestDeprecatedFeature1), "Deprecated feature should remain disabled (locked to default)")
		require.False(t, fg.Enabled(TestDeprecatedFeature2), "Deprecated feature should remain disabled (locked to default)")
	})
}

// =============================================================================
// Integration & Utilities Tests
// =============================================================================

func TestFeatureGateStringFormatting(t *testing.T) {
	// Test the string formatting that would be used by Helm templates
	fg := featuregate.NewFeatureGate()
	testGates := createTestFeatureGates()

	err := fg.Add(testGates)
	require.NoError(t, err, "fg.Add should not fail")

	// Set some features to specific values
	err = fg.SetFromMap(map[string]bool{
		string(TestAlphaFeature1): true,
		string(TestBetaFeature1):  false, // Override default
	})
	require.NoError(t, err, "SetFromMap should not fail")

	// Test that we can extract the current state for serialization (like Helm templates)
	featureStates := map[string]bool{
		string(TestAlphaFeature1): fg.Enabled(TestAlphaFeature1),
		string(TestBetaFeature1):  fg.Enabled(TestBetaFeature1),
		string(TestBetaFeature2):  fg.Enabled(TestBetaFeature2),
	}

	expected := map[string]bool{
		string(TestAlphaFeature1): true,
		string(TestBetaFeature1):  false,
		string(TestBetaFeature2):  true, // Default beta value
	}

	require.Equal(t, expected, featureStates, "Feature states should match expected values")

	// Test comma-separated format generation (like Helm templates use)
	var parts []string
	for feature, enabled := range featureStates {
		if enabled {
			parts = append(parts, feature+"=true")
		} else {
			parts = append(parts, feature+"=false")
		}
	}

	require.NotEmpty(t, parts, "Should have some feature gate parts")
	require.Contains(t, parts, string(TestAlphaFeature1)+"=true", "Should contain enabled alpha feature")
	require.Contains(t, parts, string(TestBetaFeature1)+"=false", "Should contain disabled beta feature")
}

func TestKnownFeaturesIntegration(t *testing.T) {
	t.Run("DefaultFeatureGates", func(t *testing.T) {
		fg := newFeatureGates(TestVersion)
		knownFeatures := fg.KnownFeatures()

		require.NotEmpty(t, knownFeatures, "Should have known features")

		// Verify that our real features are included
		found := false
		for _, feature := range knownFeatures {
			if strings.Contains(feature, string(TimeSlicingSettings)) {
				found = true
				break
			}
		}
		require.True(t, found, "Should contain our real features in known features list")
	})

	t.Run("TestFeatureGates", func(t *testing.T) {
		fg := featuregate.NewFeatureGate()
		testGates := createTestFeatureGates()
		err := fg.Add(testGates)
		require.NoError(t, err, "Should be able to add test feature gates")

		knownFeatures := fg.KnownFeatures()
		require.NotEmpty(t, knownFeatures, "Should have known test features")

		// Verify that test features are included and properly formatted
		testFeatureFound := false
		for _, feature := range knownFeatures {
			if strings.Contains(feature, string(TestAlphaFeature1)) {
				testFeatureFound = true
				require.Contains(t, feature, "ALPHA", "Alpha features should be marked as ALPHA in known features")
				break
			}
		}
		require.True(t, testFeatureFound, "Should contain test features in known features list")
	})
}

// =============================================================================
// Edge Cases & Error Handling Tests
// =============================================================================

func TestFeatureGateErrorHandling(t *testing.T) {
	t.Run("InvalidFeatureNames", func(t *testing.T) {
		fg := featuregate.NewFeatureGate()
		testGates := createTestFeatureGates()

		err := fg.Add(testGates)
		require.NoError(t, err, "fg.Add should not fail")

		// Test with invalid feature names
		err = fg.SetFromMap(map[string]bool{
			"NonExistentFeature": true,
		})
		require.Error(t, err, "Should fail when setting unknown feature")

		// Test multiple invalid features
		err = fg.SetFromMap(map[string]bool{
			"Invalid1": true,
			"Invalid2": false,
		})
		require.Error(t, err, "Should fail when setting multiple unknown features")
	})

	t.Run("RecoveryAfterError", func(t *testing.T) {
		// Test that a fresh feature gate works correctly after we've seen errors with another one
		fg1 := featuregate.NewFeatureGate()
		testGates := createTestFeatureGates()
		err := fg1.Add(testGates)
		require.NoError(t, err, "fg.Add should not fail")

		// Cause an error on first feature gate
		err = fg1.SetFromMap(map[string]bool{
			"NonExistentFeature": true,
		})
		require.Error(t, err, "Should fail with invalid feature")

		// Create a fresh feature gate and verify it works correctly
		fg2 := featuregate.NewFeatureGate()
		err = fg2.Add(testGates)
		require.NoError(t, err, "Should be able to add test gates to fresh feature gate")

		err = fg2.SetFromMap(map[string]bool{
			string(TestAlphaFeature1): true,
		})
		require.NoError(t, err, "Should work with valid features in fresh feature gate")
		require.True(t, fg2.Enabled(TestAlphaFeature1), "Feature should be enabled in fresh feature gate")
	})

	t.Run("MixedValidInvalidFeatures", func(t *testing.T) {
		fg := featuregate.NewFeatureGate()
		testGates := createTestFeatureGates()
		err := fg.Add(testGates)
		require.NoError(t, err, "fg.Add should not fail")

		// Test mixed valid and invalid features - should fail entirely
		err = fg.SetFromMap(map[string]bool{
			string(TestAlphaFeature1): true,  // Valid
			"NonExistentFeature":      false, // Invalid
		})
		require.Error(t, err, "Should fail when mixing valid and invalid features")

		// Original state should be preserved
		require.False(t, fg.Enabled(TestAlphaFeature1), "Feature state should be unchanged after failed mixed operation")
	})
}

// =============================================================================
// Feature Gate Validation Tests
// =============================================================================

func TestValidateFeatureGates(t *testing.T) {
	tests := []struct {
		name         string
		fgMap        map[featuregate.Feature]bool
		expectError  bool
		errorMessage string
		description  string
	}{
		{
			name:        "No features enabled",
			fgMap:       map[featuregate.Feature]bool{},
			expectError: false,
			description: "should be valid when no features are enabled",
		},
		{
			name:        "CDCliques enabled with DNSNames enabled",
			fgMap:       map[featuregate.Feature]bool{ComputeDomainCliques: true, IMEXDaemonsWithDNSNames: true},
			expectError: false,
			description: "should be valid when both ComputeDomainCliques and IMEXDaemonsWithDNSNames are enabled",
		},
		{
			name:         "CDCliques enabled without DNSNames",
			fgMap:        map[featuregate.Feature]bool{ComputeDomainCliques: true, IMEXDaemonsWithDNSNames: false},
			expectError:  true,
			errorMessage: "feature gate ComputeDomainCliques requires IMEXDaemonsWithDNSNames to also be enabled",
			description:  "should fail when ComputeDomainCliques is enabled but IMEXDaemonsWithDNSNames is not",
		},
		{
			name:        "DNSNames enabled without CDCliques",
			fgMap:       map[featuregate.Feature]bool{IMEXDaemonsWithDNSNames: true, ComputeDomainCliques: false},
			expectError: false,
			description: "should be valid when IMEXDaemonsWithDNSNames is enabled but ComputeDomainCliques is not",
		},
		{
			name:         "DynamicMIG enabled with PassthroughSupport",
			fgMap:        map[featuregate.Feature]bool{DynamicMIG: true, PassthroughSupport: true},
			expectError:  true,
			errorMessage: "feature gate DynamicMIG is currently mutually exclusive with PassthroughSupport",
			description:  "should fail when both DynamicMIG and PassthroughSupport are enabled",
		},
		{
			name:        "DynamicMIG enabled with NVMLDeviceHealthCheck",
			fgMap:       map[featuregate.Feature]bool{DynamicMIG: true, NVMLDeviceHealthCheck: true},
			expectError: false,
			description: "should allow DynamicMIG health monitoring",
		},
		{
			name:         "DynamicMIG enabled with MPSSupport",
			fgMap:        map[featuregate.Feature]bool{DynamicMIG: true, MPSSupport: true},
			expectError:  true,
			errorMessage: "feature gate DynamicMIG is currently mutually exclusive with MPSSupport",
			description:  "should fail when both DynamicMIG and MPSSupport are enabled",
		},
		{
			name:        "Only DynamicMIG enabled",
			fgMap:       map[featuregate.Feature]bool{DynamicMIG: true, PassthroughSupport: false, NVMLDeviceHealthCheck: false, MPSSupport: false},
			expectError: false,
			description: "should be valid when only DynamicMIG is enabled",
		},
		{
			name:         "PassthroughSupport enabled with NVMLDeviceHealthCheck",
			fgMap:        map[featuregate.Feature]bool{PassthroughSupport: true, NVMLDeviceHealthCheck: true},
			expectError:  true,
			errorMessage: "feature gate PassthroughSupport is currently mutually exclusive with NVMLDeviceHealthCheck",
			description:  "should fail when both PassthroughSupport and NVMLDeviceHealthCheck are enabled",
		},
		{
			name:        "Only PassthroughSupport enabled",
			fgMap:       map[featuregate.Feature]bool{PassthroughSupport: true, DynamicMIG: false, NVMLDeviceHealthCheck: false, DeviceMetadata: false},
			expectError: false,
			description: "should be valid when only PassthroughSupport is enabled",
		},
		{
			name:         "DeviceMetadata enabled without PassthroughSupport",
			fgMap:        map[featuregate.Feature]bool{DeviceMetadata: true, PassthroughSupport: false},
			expectError:  true,
			errorMessage: "feature gate DeviceMetadata requires PassthroughSupport to also be enabled",
			description:  "should fail when DeviceMetadata is enabled but PassthroughSupport is not",
		},
		{
			name:        "DeviceMetadata enabled with PassthroughSupport",
			fgMap:       map[featuregate.Feature]bool{DeviceMetadata: true, PassthroughSupport: true},
			expectError: false,
			description: "should be valid when both DeviceMetadata and PassthroughSupport are enabled",
		},
		{
			name:        "FabricManagerPartitioning enabled with PassthroughSupport",
			fgMap:       map[featuregate.Feature]bool{FabricManagerPartitioning: true, PassthroughSupport: true},
			expectError: false,
			description: "should be valid when both FabricManagerPartitioning and PassthroughSupport are enabled",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create feature gates with test configuration
			fg := newFeatureGates(TestVersion)
			fgMap := make(map[string]bool)
			for feature, state := range tt.fgMap {
				fgMap[string(feature)] = state
			}
			err := fg.SetFromMap(fgMap)
			require.NoError(t, err, "SetFromMap should not fail")

			// Temporarily replace global feature gates for validation
			oldGates := featureGates
			featureGates = fg
			defer func() { featureGates = oldGates }()

			// Validate feature gates
			err = ValidateFeatureGates()

			if tt.expectError {
				require.Error(t, err, tt.description)
				require.Contains(t, err.Error(), tt.errorMessage, "expected error message %q but got %q", tt.errorMessage, err.Error())
			} else {
				require.NoError(t, err, tt.description)
			}
		})
	}
}
