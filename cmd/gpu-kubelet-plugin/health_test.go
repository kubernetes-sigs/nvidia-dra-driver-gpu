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
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"
	"k8s.io/dynamic-resource-allocation/kubeletplugin"
	drapb "k8s.io/kubelet/pkg/apis/dra/v1beta1"
	registerapi "k8s.io/kubelet/pkg/apis/pluginregistration/v1"
)

// fakeRegistrationClient is a minimal registerapi.RegistrationClient stub whose
// GetInfo behavior is driven by getInfoErr.
type fakeRegistrationClient struct {
	getInfoErr error
}

func (f *fakeRegistrationClient) GetInfo(ctx context.Context, in *registerapi.InfoRequest, opts ...grpc.CallOption) (*registerapi.PluginInfo, error) {
	if f.getInfoErr != nil {
		return nil, f.getInfoErr
	}
	return &registerapi.PluginInfo{}, nil
}

func (f *fakeRegistrationClient) NotifyRegistrationStatus(ctx context.Context, in *registerapi.RegistrationStatus, opts ...grpc.CallOption) (*registerapi.RegistrationStatusResponse, error) {
	return &registerapi.RegistrationStatusResponse{}, nil
}

// fakeDRAPluginClient is a minimal drapb.DRAPluginClient stub whose
// NodePrepareResources behavior is driven by prepareErr.
type fakeDRAPluginClient struct {
	prepareErr error
}

func (f *fakeDRAPluginClient) NodePrepareResources(ctx context.Context, in *drapb.NodePrepareResourcesRequest, opts ...grpc.CallOption) (*drapb.NodePrepareResourcesResponse, error) {
	if f.prepareErr != nil {
		return nil, f.prepareErr
	}
	return &drapb.NodePrepareResourcesResponse{}, nil
}

func (f *fakeDRAPluginClient) NodeUnprepareResources(ctx context.Context, in *drapb.NodeUnprepareResourcesRequest, opts ...grpc.CallOption) (*drapb.NodeUnprepareResourcesResponse, error) {
	return &drapb.NodeUnprepareResourcesResponse{}, nil
}

func TestHealthcheckCheck(t *testing.T) {
	tests := []struct {
		name       string
		service    string
		getInfoErr error
		prepareErr error
		wantStatus grpc_health_v1.HealthCheckResponse_ServingStatus
		wantCode   codes.Code // codes.OK means no error expected
	}{
		{
			name:     "unknown service is rejected",
			service:  "bogus",
			wantCode: codes.NotFound,
		},
		{
			name:       "GetInfo failure reports NOT_SERVING",
			service:    "liveness",
			getInfoErr: fmt.Errorf("registration socket down"),
			wantStatus: grpc_health_v1.HealthCheckResponse_NOT_SERVING,
			wantCode:   codes.OK,
		},
		{
			name:       "NodePrepareResources failure reports NOT_SERVING",
			service:    "liveness",
			prepareErr: fmt.Errorf("dra socket down"),
			wantStatus: grpc_health_v1.HealthCheckResponse_NOT_SERVING,
			wantCode:   codes.OK,
		},
		{
			name:       "healthy plugin reports SERVING",
			service:    "",
			wantStatus: grpc_health_v1.HealthCheckResponse_SERVING,
			wantCode:   codes.OK,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := &healthcheck{
				regClient: &fakeRegistrationClient{getInfoErr: tc.getInfoErr},
				draClient: &fakeDRAPluginClient{prepareErr: tc.prepareErr},
				// Zero-value Helper: RegistrationStatus() is nil-safe.
				kphelper: &kubeletplugin.Helper{},
			}

			resp, err := h.Check(context.Background(), &grpc_health_v1.HealthCheckRequest{Service: tc.service})

			if tc.wantCode != codes.OK {
				require.Error(t, err)
				assert.Equal(t, tc.wantCode, status.Code(err))
				return
			}

			require.NoError(t, err)
			require.NotNil(t, resp)
			assert.Equal(t, tc.wantStatus, resp.Status)
		})
	}
}
