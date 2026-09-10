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
	"net"
	"net/url"
	"path"
	"strconv"
	"sync"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"
	"k8s.io/dynamic-resource-allocation/kubeletplugin"
	"k8s.io/klog/v2"
	drapb "k8s.io/kubelet/pkg/apis/dra/v1beta1"
	registerapi "k8s.io/kubelet/pkg/apis/pluginregistration/v1"
)

type healthcheck struct {
	grpc_health_v1.UnimplementedHealthServer

	server *grpc.Server
	wg     sync.WaitGroup

	kphelper *kubeletplugin.Helper

	regClient registerapi.RegistrationClient
	draClient drapb.DRAPluginClient
}

func startHealthcheck(ctx context.Context, config *Config, helper *kubeletplugin.Helper) (*healthcheck, error) {
	port := config.flags.healthcheckPort
	if port < 0 {
		return nil, nil
	}

	addr := net.JoinHostPort("", strconv.Itoa(port))
	lis, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("failed to listen for healthcheck service at %s: %w", addr, err)
	}

	regSockPath := (&url.URL{
		Scheme: "unix",
		// TODO: this needs to adapt when seamless upgrades
		// are enabled and the filename includes a uid.
		Path: path.Join(config.flags.kubeletRegistrarDirectoryPath, DriverName+"-reg.sock"),
	}).String()
	klog.V(6).Infof("connecting to registration socket path=%s", regSockPath)
	regConn, err := grpc.NewClient(
		regSockPath,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		return nil, fmt.Errorf("connect to registration socket: %w", err)
	}

	draSockPath := (&url.URL{
		Scheme: "unix",
		Path:   path.Join(config.DriverPluginPath(), "dra.sock"),
	}).String()
	klog.V(6).Infof("connecting to DRA socket path=%s", draSockPath)
	draConn, err := grpc.NewClient(
		draSockPath,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		return nil, fmt.Errorf("connect to DRA socket: %w", err)
	}

	server := grpc.NewServer()
	healthcheck := &healthcheck{
		server:    server,
		regClient: registerapi.NewRegistrationClient(regConn),
		draClient: drapb.NewDRAPluginClient(draConn),
		kphelper:  helper,
	}
	grpc_health_v1.RegisterHealthServer(server, healthcheck)

	healthcheck.wg.Add(1)
	go func() {
		defer healthcheck.wg.Done()
		klog.Infof("starting healthcheck service at %s", lis.Addr().String())
		if err := server.Serve(lis); err != nil {
			klog.Errorf("failed to serve healthcheck service on %s: %v", addr, err)
		}
	}()

	return healthcheck, nil
}

func (h *healthcheck) Stop() {
	if h.server != nil {
		klog.Info("Stopping healthcheck service")
		h.server.GracefulStop()
	}
	h.wg.Wait()
}

// Check implements [grpc_health_v1.HealthServer].
func (h *healthcheck) Check(ctx context.Context, req *grpc_health_v1.HealthCheckRequest) (*grpc_health_v1.HealthCheckResponse, error) {
	knownServices := map[string]struct{}{"": {}, "liveness": {}}
	if _, known := knownServices[req.GetService()]; !known {
		return nil, status.Error(codes.NotFound, "unknown service")
	}

	status := &grpc_health_v1.HealthCheckResponse{
		Status: grpc_health_v1.HealthCheckResponse_NOT_SERVING,
	}

	info, err := h.regClient.GetInfo(ctx, &registerapi.InfoRequest{})
	if err != nil {
		klog.ErrorS(err, "failed to call GetInfo")
		return status, nil
	}
	klog.V(7).Infof("Health check: successfully invoked GetInfo: %v", info)

	_, err = h.draClient.NodePrepareResources(ctx, &drapb.NodePrepareResourcesRequest{})
	if err != nil {
		klog.ErrorS(err, "failed to call NodePrepareResources")
		return status, nil
	}

	klog.V(7).Info("Health check: success: got NodePrepareResourcesResponse for noop request")
	klog.V(7).Infof("Current kubelet plugin registration status: %s", h.kphelper.RegistrationStatus())

	status.Status = grpc_health_v1.HealthCheckResponse_SERVING
	return status, nil
}
