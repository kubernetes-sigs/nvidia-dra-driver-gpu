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
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
)

// DefaultPollInterval is used by all Wait* helpers unless overridden.
const DefaultPollInterval = 2 * time.Second

// WaitForPodPhase blocks until the Pod is in one of the allowed phases or
// the timeout expires; returns the last observed phase.
func WaitForPodPhase(ctx context.Context, cs *kubernetes.Clientset, ns, name string, phases []corev1.PodPhase, timeout time.Duration) (corev1.PodPhase, error) {
	var last corev1.PodPhase
	err := wait.PollUntilContextTimeout(ctx, DefaultPollInterval, timeout, true, func(ctx context.Context) (bool, error) {
		pod, err := cs.CoreV1().Pods(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			if apierrors.IsNotFound(err) {
				return false, nil
			}
			return false, err
		}
		last = pod.Status.Phase
		for _, p := range phases {
			if last == p {
				return true, nil
			}
		}
		return false, nil
	})
	if err != nil {
		return last, fmt.Errorf("pod %s/%s never reached %v (last=%s): %w", ns, name, phases, last, err)
	}
	return last, nil
}

// WaitForPodsReady blocks until at least `min` pods matching the label
// selector are Running with all containers Ready. Returns the count seen
// at the last poll.
func WaitForPodsReady(ctx context.Context, cs *kubernetes.Clientset, ns, labelSelector string, min int, timeout time.Duration) (int, error) {
	var ready int
	err := wait.PollUntilContextTimeout(ctx, DefaultPollInterval, timeout, true, func(ctx context.Context) (bool, error) {
		list, err := cs.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: labelSelector})
		if err != nil {
			return false, err
		}
		ready = 0
		for _, p := range list.Items {
			if p.Status.Phase != corev1.PodRunning {
				continue
			}
			allReady := len(p.Status.ContainerStatuses) > 0
			for _, c := range p.Status.ContainerStatuses {
				if !c.Ready {
					allReady = false
					break
				}
			}
			if allReady {
				ready++
			}
		}
		return ready >= min, nil
	})
	return ready, err
}

// WaitForPodReady waits until a named Pod is Running and every container is
// Ready. It is useful for workloads which must remain running while their
// logs or DRA allocation are inspected.
func WaitForPodReady(ctx context.Context, cs *kubernetes.Clientset, ns, name string, timeout time.Duration) error {
	err := wait.PollUntilContextTimeout(ctx, DefaultPollInterval, timeout, true, func(ctx context.Context) (bool, error) {
		pod, err := cs.CoreV1().Pods(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			if apierrors.IsNotFound(err) {
				return false, nil
			}
			return false, err
		}
		if pod.Status.Phase != corev1.PodRunning || len(pod.Status.ContainerStatuses) == 0 {
			return false, nil
		}
		for _, status := range pod.Status.ContainerStatuses {
			if !status.Ready {
				return false, nil
			}
		}
		return true, nil
	})
	if err != nil {
		return fmt.Errorf("pod %s/%s never became Ready: %w", ns, name, err)
	}
	return nil
}
