// Copyright The Kubernetes Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0

//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	resourceapi "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"sigs.k8s.io/dra-driver-nvidia-gpu/test/e2e/framework"
)

const (
	driverNamespace   = "dra-driver-nvidia-gpu"
	kubeletPluginDS   = "dra-driver-nvidia-gpu-kubelet-plugin"
	deviceStatusClaim = "device-status-claim"
)

// deviceStatusData mirrors the JSON payload the kubelet plugin publishes into
// ResourceClaim.status.devices[].data (see cmd/gpu-kubelet-plugin/devicestatus.go).
type deviceStatusData struct {
	Type          string `json:"type"`
	UUID          string `json:"uuid"`
	ProductName   string `json:"productName"`
	DriverVersion string `json:"driverVersion"`
	PCIBusID      string `json:"pciBusID"`
	MigProfile    string `json:"migProfile"`
	ParentUUID    string `json:"parentUUID"`
}

// KEP-4817: the kubelet plugin publishes per-device status into
// ResourceClaim.status.devices when the ResourceClaimDeviceStatus driver
// feature gate is enabled.
var _ = Describe("Device Status", func() {
	var ns string

	BeforeEach(func() {
		ns = fmt.Sprintf("gpu-e2e-%d", time.Now().UnixNano()%1_000_000)
	})

	AfterEach(func(ctx SpecContext) {
		_ = cs.CoreV1().Namespaces().Delete(ctx, ns, metav1.DeleteOptions{})
	})

	It("[device-status] publishes, keeps and prunes ResourceClaim.status.devices across the claim lifecycle", func(ctx SpecContext) {
		requireDeviceStatus(ctx)

		// Prepare: one pod on a standalone claim.
		yaml, err := framework.Render("device-status", map[string]any{"Namespace": ns, "PodName": "pod1"})
		Expect(err).NotTo(HaveOccurred())
		Expect(framework.ApplyYAML(ctx, yaml)).To(Succeed())

		phase, err := framework.WaitForPodPhase(ctx, cs, ns, "pod1",
			[]corev1.PodPhase{corev1.PodRunning, corev1.PodSucceeded}, 3*time.Minute)
		Expect(err).NotTo(HaveOccurred(), "pod1 never reached Running/Succeeded (phase=%s)", phase)

		claim := waitForDeviceStatus(ctx, ns, 1, 2*time.Minute)
		Expect(claim.Status.Allocation).NotTo(BeNil())
		Expect(claim.Status.Allocation.Devices.Results).To(HaveLen(1))
		result := claim.Status.Allocation.Devices.Results[0]

		status := claim.Status.Devices[0]
		Expect(status.Driver).To(Equal("gpu.nvidia.com"))
		Expect(status.Pool).To(Equal(result.Pool))
		Expect(status.Device).To(Equal(result.Device))
		Expect(status.Data).NotTo(BeNil(), "status.devices[0].data must be set")

		var data deviceStatusData
		Expect(json.Unmarshal(status.Data.Raw, &data)).To(Succeed())
		Expect(data.Type).To(Equal("gpu"))
		Expect(data.UUID).To(HavePrefix("GPU-"))
		Expect(data.ProductName).To(Equal(gpu.ProductName))
		Expect(data.DriverVersion).NotTo(BeEmpty())
		Expect(data.PCIBusID).NotTo(BeEmpty())

		// The payload must describe the very device that was allocated,
		// as advertised in the ResourceSlice.
		Expect(data.UUID).To(Equal(publishedUUID(ctx, result.Pool, result.Device)),
			"status.devices[0].data.uuid must match the ResourceSlice uuid attribute of the allocated device")

		// A second consumer of the same claim must not disturb the entry.
		yaml, err = framework.Render("device-status", map[string]any{"Namespace": ns, "PodName": "pod2"})
		Expect(err).NotTo(HaveOccurred())
		Expect(framework.ApplyYAML(ctx, yaml)).To(Succeed())
		phase, err = framework.WaitForPodPhase(ctx, cs, ns, "pod2",
			[]corev1.PodPhase{corev1.PodRunning, corev1.PodSucceeded}, 3*time.Minute)
		Expect(err).NotTo(HaveOccurred(), "pod2 never reached Running/Succeeded (phase=%s)", phase)

		claim, err = cs.ResourceV1().ResourceClaims(ns).Get(ctx, deviceStatusClaim, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(claim.Status.Devices).To(HaveLen(1))
		Expect(claim.Status.Devices[0].Data.Raw).To(MatchJSON(status.Data.Raw))

		// Unprepare: once the last consumer is gone the entry is pruned,
		// either by the plugin (Unprepare) or by the API server when the
		// claim is deallocated.
		Expect(cs.CoreV1().Pods(ns).DeleteCollection(ctx, metav1.DeleteOptions{},
			metav1.ListOptions{LabelSelector: "app=device-status"})).To(Succeed())
		Eventually(func(g Gomega) {
			c, err := cs.ResourceV1().ResourceClaims(ns).Get(ctx, deviceStatusClaim, metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(c.Status.Devices).To(BeEmpty(), "status.devices must be pruned after unprepare/deallocation")
		}).WithContext(ctx).WithTimeout(3 * time.Minute).WithPolling(2 * time.Second).Should(Succeed())
	})
})

// requireDeviceStatus skips the spec unless the deployed kubelet plugin runs
// with the ResourceClaimDeviceStatus driver feature gate enabled (as rendered
// into the FEATURE_GATES env var by the Helm chart).
func requireDeviceStatus(ctx context.Context) {
	ds, err := cs.AppsV1().DaemonSets(driverNamespace).Get(ctx, kubeletPluginDS, metav1.GetOptions{})
	if err != nil {
		Skip(fmt.Sprintf("cannot inspect kubelet plugin DaemonSet %s/%s: %v", driverNamespace, kubeletPluginDS, err))
	}
	for _, c := range ds.Spec.Template.Spec.Containers {
		for _, e := range c.Env {
			if e.Name == "FEATURE_GATES" && strings.Contains(e.Value, "ResourceClaimDeviceStatus=true") {
				return
			}
		}
	}
	Skip("ResourceClaimDeviceStatus feature is not enabled on the deployed cluster driver")
}

// waitForDeviceStatus polls until the claim carries at least n status.devices
// entries owned by gpu.nvidia.com and returns the claim.
func waitForDeviceStatus(ctx context.Context, ns string, n int, timeout time.Duration) *resourceapi.ResourceClaim {
	var claim *resourceapi.ResourceClaim
	Eventually(func(g Gomega) {
		c, err := cs.ResourceV1().ResourceClaims(ns).Get(ctx, deviceStatusClaim, metav1.GetOptions{})
		g.Expect(err).NotTo(HaveOccurred())
		count := 0
		for _, d := range c.Status.Devices {
			if d.Driver == "gpu.nvidia.com" {
				count++
			}
		}
		g.Expect(count).To(BeNumerically(">=", n), "claim %s has %d gpu.nvidia.com status entries", c.Name, count)
		claim = c
	}).WithContext(ctx).WithTimeout(timeout).WithPolling(2 * time.Second).Should(Succeed())
	return claim
}

// publishedUUID returns the uuid attribute of the named device in the
// gpu.nvidia.com ResourceSlice for the given pool.
func publishedUUID(ctx context.Context, pool, device string) string {
	slices, err := cs.ResourceV1().ResourceSlices().List(ctx, metav1.ListOptions{})
	Expect(err).NotTo(HaveOccurred())
	for _, s := range slices.Items {
		if s.Spec.Driver != "gpu.nvidia.com" || s.Spec.Pool.Name != pool {
			continue
		}
		for _, d := range s.Spec.Devices {
			if d.Name != device {
				continue
			}
			if attr, ok := d.Attributes["uuid"]; ok && attr.StringValue != nil {
				return *attr.StringValue
			}
		}
	}
	Fail(fmt.Sprintf("device %s/%s not found in any gpu.nvidia.com ResourceSlice", pool, device))
	return ""
}
