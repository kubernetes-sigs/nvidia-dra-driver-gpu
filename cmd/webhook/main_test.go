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
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	admissionv1 "k8s.io/api/admission/v1"
	resourceapi "k8s.io/api/resource/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utilversion "k8s.io/apimachinery/pkg/util/version"
	"k8s.io/utils/ptr"

	configapi "sigs.k8s.io/dra-driver-nvidia-gpu/api/nvidia.com/resource/v1beta1"
	"sigs.k8s.io/dra-driver-nvidia-gpu/pkg/featuregates"
)

func TestReadyEndpoint(t *testing.T) {
	s := httptest.NewServer(newMux())
	t.Cleanup(s.Close)

	res, err := http.Get(s.URL + "/readyz")
	assert.NoError(t, err)
	assert.Equal(t, http.StatusOK, res.StatusCode)
}

func TestResourceClaimValidatingWebhook(t *testing.T) {

	tests := map[string]struct {
		featureGates         map[string]bool
		admissionReview      *admissionv1.AdmissionReview
		requestContentType   string
		expectedResponseCode int
		expectedAllowed      bool
		expectedMessage      string
	}{
		"bad contentType": {
			requestContentType:   "invalid type",
			expectedResponseCode: http.StatusUnsupportedMediaType,
		},
		"invalid AdmissionReview": {
			admissionReview:      &admissionv1.AdmissionReview{},
			expectedResponseCode: http.StatusBadRequest,
		},
		"valid GpuConfig in ResourceClaim": {
			featureGates: map[string]bool{
				string(featuregates.TimeSlicingSettings): true,
			},
			admissionReview: admissionReviewWithObject(
				resourceClaimWithGpuConfigs(
					resourceClaimResourceV1Beta1,
					&configapi.GpuConfig{
						Sharing: &configapi.GpuSharing{
							Strategy: configapi.TimeSlicingStrategy,
							TimeSlicingConfig: &configapi.TimeSlicingConfig{
								Interval: ptr.To(configapi.DefaultTimeSlice),
							},
						},
					},
				),
			),
			expectedAllowed: true,
		},
		"valid LocalIPCConfig in ResourceClaim": {
			featureGates: map[string]bool{
				string(featuregates.LocalIPCDirectory): true,
			},
			admissionReview: admissionReviewWithObject(
				resourceClaimWithLocalIPCConfig(resourceClaimResourceV1Beta1),
			),
			expectedAllowed: true,
		},
		"LocalIPCConfig rejected when LocalIPCDirectory feature gate is disabled": {
			featureGates: map[string]bool{
				string(featuregates.LocalIPCDirectory): false,
			},
			admissionReview: admissionReviewWithObject(
				resourceClaimWithLocalIPCConfig(resourceClaimResourceV1Beta1),
			),
			expectedAllowed: false,
			expectedMessage: `1 configs failed to validate: object at spec.devices.config[0].opaque.parameters is invalid: local IPC directory is requested, but the "LocalIPCDirectory" feature gate is not enabled`,
		},
		"LocalIPCConfig rejected when scoped to a request": {
			featureGates: map[string]bool{
				string(featuregates.LocalIPCDirectory): true,
			},
			admissionReview: admissionReviewWithObject(
				resourceClaimWithLocalIPCConfig(resourceClaimResourceV1Beta1, "gpu"),
			),
			expectedAllowed: false,
			expectedMessage: "1 configs failed to validate: local IPC config at spec.devices.config[0].requests must not select individual requests",
		},
		"invalid GpuConfigs in ResourceClaim": {
			featureGates: map[string]bool{
				string(featuregates.TimeSlicingSettings): true,
				string(featuregates.MPSSupport):          true,
			},
			admissionReview: admissionReviewWithObject(
				resourceClaimWithGpuConfigs(
					resourceClaimResourceV1Beta1,
					&configapi.GpuConfig{
						Sharing: &configapi.GpuSharing{
							Strategy: configapi.TimeSlicingStrategy,
							TimeSlicingConfig: &configapi.TimeSlicingConfig{
								Interval: ptr.To(configapi.TimeSliceInterval("Invalid Interval")),
							},
						},
					},
					&configapi.GpuConfig{
						Sharing: &configapi.GpuSharing{
							Strategy: configapi.MpsStrategy,
							MpsConfig: &configapi.MpsConfig{
								DefaultActiveThreadPercentage: ptr.To(-1),
							},
						},
					},
				),
			),
			expectedAllowed: false,
			expectedMessage: "2 configs failed to validate: object at spec.devices.config[0].opaque.parameters is invalid: unknown time-slice interval: Invalid Interval, supported time-slice intervals: Default, Short, Medium, Long; object at spec.devices.config[1].opaque.parameters is invalid: active thread percentage must not be negative",
		},
		"valid ComputeDomainDaemonConfig in ResourceClaim": {
			admissionReview: admissionReviewWithObject(
				resourceClaimWithComputeDomainDaemonConfig(
					resourceClaimResourceV1Beta1,
					&configapi.ComputeDomainDaemonConfig{
						DomainID: "a1b2c3d4-e5f6-7890-abcd-ef1234567890",
					},
				),
			),
			expectedAllowed: true,
		},
		"path traversal domainID in ComputeDomainDaemonConfig is rejected": {
			admissionReview: admissionReviewWithObject(
				resourceClaimWithComputeDomainDaemonConfig(
					resourceClaimResourceV1Beta1,
					&configapi.ComputeDomainDaemonConfig{
						DomainID: "../../../../../../escape-target/pwn",
					},
				),
			),
			expectedAllowed: false,
			expectedMessage: `1 configs failed to validate: object at spec.devices.config[0].opaque.parameters is invalid: domainID "../../../../../../escape-target/pwn" is not a valid single path component: may not contain '/'`,
		},
		"valid GpuConfig in ResourceClaimTemplate": {
			featureGates: map[string]bool{
				string(featuregates.TimeSlicingSettings): true,
			},
			admissionReview: admissionReviewWithObject(
				resourceClaimTemplateWithGpuConfigs(
					resourceClaimTemplateResourceV1Beta1,
					&configapi.GpuConfig{
						Sharing: &configapi.GpuSharing{
							Strategy: configapi.TimeSlicingStrategy,
							TimeSlicingConfig: &configapi.TimeSlicingConfig{
								Interval: ptr.To(configapi.DefaultTimeSlice),
							},
						},
					},
				),
			),
			expectedAllowed: true,
		},
		"invalid GpuConfigs in ResourceClaimTemplate": {
			admissionReview: admissionReviewWithObject(
				resourceClaimTemplateWithGpuConfigs(
					resourceClaimTemplateResourceV1Beta1,
					&configapi.GpuConfig{
						Sharing: &configapi.GpuSharing{
							Strategy: configapi.TimeSlicingStrategy,
							TimeSlicingConfig: &configapi.TimeSlicingConfig{
								Interval: ptr.To(configapi.TimeSliceInterval("Invalid Interval")),
							},
						},
					},
					&configapi.GpuConfig{
						Sharing: &configapi.GpuSharing{
							Strategy: configapi.MpsStrategy,
							MpsConfig: &configapi.MpsConfig{
								DefaultActiveThreadPercentage: ptr.To(-1),
							},
						},
					},
				),
			),
			expectedAllowed: false,
			expectedMessage: "2 configs failed to validate: object at spec.spec.devices.config[0].opaque.parameters is invalid: unknown time-slice interval: Invalid Interval, supported time-slice intervals: Default, Short, Medium, Long; object at spec.spec.devices.config[1].opaque.parameters is invalid: active thread percentage must not be negative",
			featureGates: map[string]bool{
				string(featuregates.TimeSlicingSettings): true,
				string(featuregates.MPSSupport):          true,
			},
		},

		// v1 API version tests
		"valid GpuConfig in ResourceClaim v1": {
			admissionReview: admissionReviewWithObject(
				resourceClaimWithGpuConfigs(
					resourceClaimResourceV1,
					&configapi.GpuConfig{
						Sharing: &configapi.GpuSharing{
							Strategy: configapi.TimeSlicingStrategy,
							TimeSlicingConfig: &configapi.TimeSlicingConfig{
								Interval: ptr.To(configapi.DefaultTimeSlice),
							},
						},
					},
				),
			),
			expectedAllowed: true,
			featureGates: map[string]bool{
				string(featuregates.TimeSlicingSettings): true,
			},
		},
		"valid GpuConfig in ResourceClaimTemplate v1": {
			featureGates: map[string]bool{
				string(featuregates.TimeSlicingSettings): true,
			},
			admissionReview: admissionReviewWithObject(
				resourceClaimTemplateWithGpuConfigs(
					resourceClaimTemplateResourceV1,
					&configapi.GpuConfig{
						Sharing: &configapi.GpuSharing{
							Strategy: configapi.TimeSlicingStrategy,
							TimeSlicingConfig: &configapi.TimeSlicingConfig{
								Interval: ptr.To(configapi.DefaultTimeSlice),
							},
						},
					},
				),
			),
			expectedAllowed: true,
		},
		"invalid GpuConfig in ResourceClaim v1 (tests conversion)": {
			featureGates: map[string]bool{
				string(featuregates.TimeSlicingSettings): true,
			},
			admissionReview: admissionReviewWithObject(
				resourceClaimWithGpuConfigs(
					resourceClaimResourceV1,
					&configapi.GpuConfig{
						Sharing: &configapi.GpuSharing{
							Strategy: configapi.TimeSlicingStrategy,
							TimeSlicingConfig: &configapi.TimeSlicingConfig{
								Interval: ptr.To(configapi.TimeSliceInterval("Invalid Interval")),
							},
						},
					},
				),
			),
			expectedAllowed: false,
			expectedMessage: "1 configs failed to validate: object at spec.devices.config[0].opaque.parameters is invalid: unknown time-slice interval: Invalid Interval, supported time-slice intervals: Default, Short, Medium, Long",
		},
		"invalid GpuConfig in ResourceClaimTemplate v1 (tests conversion)": {
			featureGates: map[string]bool{
				string(featuregates.MPSSupport): true,
			},
			admissionReview: admissionReviewWithObject(
				resourceClaimTemplateWithGpuConfigs(
					resourceClaimTemplateResourceV1,
					&configapi.GpuConfig{
						Sharing: &configapi.GpuSharing{
							Strategy: configapi.MpsStrategy,
							MpsConfig: &configapi.MpsConfig{
								DefaultActiveThreadPercentage: ptr.To(-1),
							},
						},
					},
				),
			),
			expectedAllowed: false,
			expectedMessage: "1 configs failed to validate: object at spec.spec.devices.config[0].opaque.parameters is invalid: active thread percentage must not be negative",
		},

		// v1beta2 API version tests
		"valid GpuConfig in ResourceClaim v1beta2": {
			featureGates: map[string]bool{
				string(featuregates.TimeSlicingSettings): true,
			},
			admissionReview: admissionReviewWithObject(
				resourceClaimWithGpuConfigs(
					resourceClaimResourceV1Beta2,
					&configapi.GpuConfig{
						Sharing: &configapi.GpuSharing{
							Strategy: configapi.TimeSlicingStrategy,
							TimeSlicingConfig: &configapi.TimeSlicingConfig{
								Interval: ptr.To(configapi.DefaultTimeSlice),
							},
						},
					},
				),
			),
			expectedAllowed: true,
		},
		"valid GpuConfig in ResourceClaimTemplate v1beta2": {
			featureGates: map[string]bool{
				string(featuregates.TimeSlicingSettings): true,
			},
			admissionReview: admissionReviewWithObject(
				resourceClaimTemplateWithGpuConfigs(
					resourceClaimTemplateResourceV1Beta2,
					&configapi.GpuConfig{
						Sharing: &configapi.GpuSharing{
							Strategy: configapi.TimeSlicingStrategy,
							TimeSlicingConfig: &configapi.TimeSlicingConfig{
								Interval: ptr.To(configapi.DefaultTimeSlice),
							},
						},
					},
				),
			),
			expectedAllowed: true,
		},
		"invalid GpuConfig in ResourceClaim v1beta2 (tests conversion)": {
			featureGates: map[string]bool{
				string(featuregates.MPSSupport): true,
			},
			admissionReview: admissionReviewWithObject(
				resourceClaimWithGpuConfigs(
					resourceClaimResourceV1Beta2,
					&configapi.GpuConfig{
						Sharing: &configapi.GpuSharing{
							Strategy: configapi.MpsStrategy,
							MpsConfig: &configapi.MpsConfig{
								DefaultActiveThreadPercentage: ptr.To(-1),
							},
						},
					},
				),
			),
			expectedAllowed: false,
			expectedMessage: "1 configs failed to validate: object at spec.devices.config[0].opaque.parameters is invalid: active thread percentage must not be negative",
		},
		"invalid GpuConfig in ResourceClaimTemplate v1beta2 (tests conversion)": {
			featureGates: map[string]bool{
				string(featuregates.TimeSlicingSettings): true,
			},
			admissionReview: admissionReviewWithObject(
				resourceClaimTemplateWithGpuConfigs(
					resourceClaimTemplateResourceV1Beta2,
					&configapi.GpuConfig{
						Sharing: &configapi.GpuSharing{
							Strategy: configapi.TimeSlicingStrategy,
							TimeSlicingConfig: &configapi.TimeSlicingConfig{
								Interval: ptr.To(configapi.TimeSliceInterval("Invalid Interval")),
							},
						},
					},
				),
			),
			expectedAllowed: false,
			expectedMessage: "1 configs failed to validate: object at spec.spec.devices.config[0].opaque.parameters is invalid: unknown time-slice interval: Invalid Interval, supported time-slice intervals: Default, Short, Medium, Long",
		},

		// Feature gate disabled tests - these should fail when feature gates are off
		"TimeSlicingStrategy rejected when TimeSlicingSettings feature gate is disabled": {
			featureGates: map[string]bool{
				string(featuregates.TimeSlicingSettings): false,
			},
			admissionReview: admissionReviewWithObject(
				resourceClaimWithGpuConfigs(
					resourceClaimResourceV1Beta1,
					&configapi.GpuConfig{
						Sharing: &configapi.GpuSharing{
							Strategy: configapi.TimeSlicingStrategy,
							TimeSlicingConfig: &configapi.TimeSlicingConfig{
								Interval: ptr.To(configapi.DefaultTimeSlice),
							},
						},
					},
				),
			),
			expectedAllowed: false,
			expectedMessage: `1 configs failed to validate: object at spec.devices.config[0].opaque.parameters is invalid: "TimeSlicing" is selected as the GPU sharing strategy, but the "TimeSlicingSettings" feature gate is not enabled`,
		},
		"MpsStrategy rejected when MPSSupport feature gate is disabled": {
			featureGates: map[string]bool{
				string(featuregates.MPSSupport): false,
			},
			admissionReview: admissionReviewWithObject(
				resourceClaimWithGpuConfigs(
					resourceClaimResourceV1Beta1,
					&configapi.GpuConfig{
						Sharing: &configapi.GpuSharing{
							Strategy: configapi.MpsStrategy,
							MpsConfig: &configapi.MpsConfig{
								DefaultActiveThreadPercentage: ptr.To(50),
							},
						},
					},
				),
			),
			expectedAllowed: false,
			expectedMessage: `1 configs failed to validate: object at spec.devices.config[0].opaque.parameters is invalid: "MPS" is selected as the GPU sharing strategy, but the "MPSSupport" feature gate is not enabled`,
		},
		"TimeSlicingStrategy rejected in ResourceClaimTemplate when TimeSlicingSettings feature gate is disabled": {
			featureGates: map[string]bool{
				string(featuregates.TimeSlicingSettings): false,
			},
			admissionReview: admissionReviewWithObject(
				resourceClaimTemplateWithGpuConfigs(
					resourceClaimTemplateResourceV1Beta1,
					&configapi.GpuConfig{
						Sharing: &configapi.GpuSharing{
							Strategy: configapi.TimeSlicingStrategy,
							TimeSlicingConfig: &configapi.TimeSlicingConfig{
								Interval: ptr.To(configapi.DefaultTimeSlice),
							},
						},
					},
				),
			),
			expectedAllowed: false,
			expectedMessage: `1 configs failed to validate: object at spec.spec.devices.config[0].opaque.parameters is invalid: "TimeSlicing" is selected as the GPU sharing strategy, but the "TimeSlicingSettings" feature gate is not enabled`,
		},
		"MpsStrategy rejected in ResourceClaimTemplate when MPSSupport feature gate is disabled": {
			featureGates: map[string]bool{
				string(featuregates.MPSSupport): false,
			},
			admissionReview: admissionReviewWithObject(
				resourceClaimTemplateWithGpuConfigs(
					resourceClaimTemplateResourceV1Beta1,
					&configapi.GpuConfig{
						Sharing: &configapi.GpuSharing{
							Strategy: configapi.MpsStrategy,
							MpsConfig: &configapi.MpsConfig{
								DefaultActiveThreadPercentage: ptr.To(50),
							},
						},
					},
				),
			),
			expectedAllowed: false,
			expectedMessage: `1 configs failed to validate: object at spec.spec.devices.config[0].opaque.parameters is invalid: "MPS" is selected as the GPU sharing strategy, but the "MPSSupport" feature gate is not enabled`,
		},
	}

	s := httptest.NewServer(newMux())
	t.Cleanup(s.Close)

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			err := featuregates.FeatureGates().SetEmulationVersion(utilversion.MajorMinor(999, 999))
			require.NoError(t, err)

			if test.featureGates != nil {
				err := featuregates.FeatureGates().SetFromMap(test.featureGates)
				require.NoError(t, err)
			}

			requestBody, err := json.Marshal(test.admissionReview)
			require.NoError(t, err)

			contentType := test.requestContentType
			if contentType == "" {
				contentType = "application/json"
			}

			res, err := http.Post(s.URL+"/validate-resource-claim-parameters", contentType, bytes.NewReader(requestBody))
			require.NoError(t, err)
			expectedResponseCode := test.expectedResponseCode
			if expectedResponseCode == 0 {
				expectedResponseCode = http.StatusOK
			}
			assert.Equal(t, expectedResponseCode, res.StatusCode)
			if res.StatusCode != http.StatusOK {
				// We don't have an AdmissionReview to validate
				return
			}

			responseBody, err := io.ReadAll(res.Body)
			require.NoError(t, err)
			res.Body.Close()

			responseAdmissionReview, err := readAdmissionReview(responseBody)
			assert.NoError(t, err)
			assert.Equal(t, test.expectedAllowed, responseAdmissionReview.Response.Allowed)
			if !test.expectedAllowed {
				assert.Equal(t, test.expectedMessage, string(responseAdmissionReview.Response.Result.Message))
			}
		})
	}
}

func admissionReviewWithObject(obj runtime.Object) *admissionv1.AdmissionReview {
	// Extract GVR from the object's GVK
	gvk := obj.GetObjectKind().GroupVersionKind()
	resource := metav1.GroupVersionResource{
		Group:    gvk.Group,
		Version:  gvk.Version,
		Resource: strings.ToLower(gvk.Kind) + "s", // Convert Kind to resource name
	}

	requestedAdmissionReview := &admissionv1.AdmissionReview{
		Request: &admissionv1.AdmissionRequest{
			Resource: resource,
			Object: runtime.RawExtension{
				Object: obj,
			},
		},
	}
	requestedAdmissionReview.SetGroupVersionKind(admissionv1.SchemeGroupVersion.WithKind("AdmissionReview"))
	return requestedAdmissionReview
}

func resourceClaimWithGpuConfigs(gvr metav1.GroupVersionResource, gpuConfigs ...*configapi.GpuConfig) *resourceapi.ResourceClaim {
	resourceClaim := &resourceapi.ResourceClaim{
		TypeMeta: metav1.TypeMeta{
			APIVersion: gvr.Group + "/" + gvr.Version,
			Kind:       "ResourceClaim",
		},
		Spec: resourceClaimSpecWithGpuConfigs(gpuConfigs...),
	}
	return resourceClaim
}

func resourceClaimWithLocalIPCConfig(gvr metav1.GroupVersionResource, requests ...string) *resourceapi.ResourceClaim {
	config := &configapi.LocalIPCConfig{Enabled: true}
	config.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   configapi.GroupName,
		Version: configapi.Version,
		Kind:    configapi.LocalIPCConfigKind,
	})
	return &resourceapi.ResourceClaim{
		TypeMeta: metav1.TypeMeta{
			APIVersion: gvr.Group + "/" + gvr.Version,
			Kind:       "ResourceClaim",
		},
		Spec: resourceapi.ResourceClaimSpec{
			Devices: resourceapi.DeviceClaim{
				Config: []resourceapi.DeviceClaimConfiguration{{
					Requests: requests,
					DeviceConfiguration: resourceapi.DeviceConfiguration{
						Opaque: &resourceapi.OpaqueDeviceConfiguration{
							Driver: DriverName,
							Parameters: runtime.RawExtension{
								Object: config,
							},
						},
					},
				}},
			},
		},
	}
}

func resourceClaimTemplateWithGpuConfigs(gvr metav1.GroupVersionResource, gpuConfigs ...*configapi.GpuConfig) *resourceapi.ResourceClaimTemplate {
	resourceClaimTemplate := &resourceapi.ResourceClaimTemplate{
		TypeMeta: metav1.TypeMeta{
			APIVersion: gvr.Group + "/" + gvr.Version,
			Kind:       "ResourceClaimTemplate",
		},
		Spec: resourceapi.ResourceClaimTemplateSpec{
			Spec: resourceClaimSpecWithGpuConfigs(gpuConfigs...),
		},
	}
	return resourceClaimTemplate
}

func resourceClaimWithComputeDomainDaemonConfig(gvr metav1.GroupVersionResource, config *configapi.ComputeDomainDaemonConfig) *resourceapi.ResourceClaim {
	config.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   configapi.GroupName,
		Version: configapi.Version,
		Kind:    "ComputeDomainDaemonConfig",
	})
	return &resourceapi.ResourceClaim{
		TypeMeta: metav1.TypeMeta{
			APIVersion: gvr.Group + "/" + gvr.Version,
			Kind:       "ResourceClaim",
		},
		Spec: resourceapi.ResourceClaimSpec{
			Devices: resourceapi.DeviceClaim{
				Config: []resourceapi.DeviceClaimConfiguration{
					{
						DeviceConfiguration: resourceapi.DeviceConfiguration{
							Opaque: &resourceapi.OpaqueDeviceConfiguration{
								Driver: ComputeDomainDriverName,
								Parameters: runtime.RawExtension{
									Object: config,
								},
							},
						},
					},
				},
			},
		},
	}
}

func resourceClaimSpecWithGpuConfigs(gpuConfigs ...*configapi.GpuConfig) resourceapi.ResourceClaimSpec {
	resourceClaimSpec := resourceapi.ResourceClaimSpec{}
	for _, gpuConfig := range gpuConfigs {
		gpuConfig.SetGroupVersionKind(schema.GroupVersionKind{
			Group:   configapi.GroupName,
			Version: configapi.Version,
			Kind:    "GpuConfig",
		})
		deviceConfig := resourceapi.DeviceClaimConfiguration{
			DeviceConfiguration: resourceapi.DeviceConfiguration{
				Opaque: &resourceapi.OpaqueDeviceConfiguration{
					Driver: DriverName,
					Parameters: runtime.RawExtension{
						Object: gpuConfig,
					},
				},
			},
		}
		resourceClaimSpec.Devices.Config = append(resourceClaimSpec.Devices.Config, deviceConfig)
	}
	return resourceClaimSpec
}
