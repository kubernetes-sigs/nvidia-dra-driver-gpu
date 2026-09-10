// Copyright The Kubernetes Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0

package framework

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// GPUDetails carries the device attributes the tests use to build CEL
// selectors: product name, driver version, and memory capacity. Populated
// by DetectGPU from the first gpu.nvidia.com ResourceSlice device.
type GPUDetails struct {
	NodeName      string
	ProductName   string // e.g. "Tesla T4"
	DriverVersion string // e.g. "580.126.9" (leading zeros stripped)
	Memory        resource.Quantity
	MemoryGiB     int64 // floor(Memory / Gi) for threshold arithmetic
}

// DetectGPU lists ResourceSlices from the gpu.nvidia.com driver and returns
// details of the first device found. Fails fast if the driver has not yet
// published any slice.
func DetectGPU(ctx context.Context, cs *kubernetes.Clientset) (*GPUDetails, error) {
	slices, err := cs.ResourceV1().ResourceSlices().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list ResourceSlices: %w", err)
	}
	for _, s := range slices.Items {
		if s.Spec.Driver != "gpu.nvidia.com" {
			continue
		}
		for _, d := range s.Spec.Devices {
			details := &GPUDetails{}
			if s.Spec.NodeName != nil {
				details.NodeName = *s.Spec.NodeName
			}
			if attr, ok := d.Attributes["productName"]; ok && attr.StringValue != nil {
				details.ProductName = *attr.StringValue
			}
			if attr, ok := d.Attributes["driverVersion"]; ok && attr.VersionValue != nil {
				details.DriverVersion = StripSemverLeadingZeros(*attr.VersionValue)
			}
			if cap, ok := d.Capacity["memory"]; ok {
				details.Memory = cap.Value
				gi := resource.MustParse("1Gi")
				details.MemoryGiB = details.Memory.Value() / gi.Value()
			}
			if details.ProductName == "" {
				return nil, fmt.Errorf("ResourceSlice %s device %s missing productName", s.Name, d.Name)
			}
			return details, nil
		}
	}
	return nil, fmt.Errorf("no gpu.nvidia.com ResourceSlice with a device found")
}

// HasDeviceType reports whether the NVIDIA GPU driver currently advertises a
// device with the requested type (for example, "gpu" or "mig").
func HasDeviceType(ctx context.Context, cs *kubernetes.Clientset, deviceType string) (bool, error) {
	slices, err := cs.ResourceV1().ResourceSlices().List(ctx, metav1.ListOptions{})
	if err != nil {
		return false, fmt.Errorf("list ResourceSlices: %w", err)
	}
	for _, s := range slices.Items {
		if s.Spec.Driver != "gpu.nvidia.com" {
			continue
		}
		for _, d := range s.Spec.Devices {
			attr, ok := d.Attributes["type"]
			if ok && attr.StringValue != nil && *attr.StringValue == deviceType {
				return true, nil
			}
		}
	}
	return false, nil
}

// StripSemverLeadingZeros normalizes "580.105.04" to "580.105.4" so CEL's
// semver() parser accepts it (it rejects leading zeros).
func StripSemverLeadingZeros(v string) string {
	re := regexp.MustCompile(`(^|\.)0+(\d)`)
	out := v
	for {
		n := re.ReplaceAllString(out, "${1}${2}")
		if n == out {
			return n
		}
		out = n
	}
}

// LowerKebab lowercases and converts '-' to ' ', converting an NFD label
// value like "Tesla-T4" to the form CEL's lowerAscii() productName returns.
func LowerKebab(s string) string {
	return strings.ToLower(strings.ReplaceAll(s, "-", " "))
}
