// Copyright The Kubernetes Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

//go:build e2e

package e2e

import (
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"sigs.k8s.io/dra-driver-nvidia-gpu/test/e2e/framework"
)

var _ = Describe("GPU workloads", Label("gpu-workloads"), func() {
	var ns string

	BeforeEach(func(ctx SpecContext) {
		ns = fmt.Sprintf("gpu-e2e-workload-%d", time.Now().UnixNano()%1_000_000)
		Expect(framework.CreateNamespace(ctx, cs, ns)).To(Succeed())
	})

	AfterEach(func(ctx SpecContext) {
		Expect(cs.CoreV1().Namespaces().Delete(ctx, ns, metav1.DeleteOptions{})).To(Succeed())
	})

	It("allocates one full GPU to a pod", Label("fastfeedback"), func(ctx SpecContext) {
		yaml, err := framework.Render("gpu-simple-full", map[string]any{"Namespace": ns})
		Expect(err).NotTo(HaveOccurred())
		Expect(framework.ApplyYAML(ctx, yaml)).To(Succeed())

		Expect(framework.WaitForPodReady(ctx, cs, ns, "pod-full-gpu", 3*time.Minute)).To(Succeed())

		logs, err := framework.PodLogs(ctx, cs, ns, "pod-full-gpu", "ctr")
		Expect(err).NotTo(HaveOccurred())
		Expect(logs).To(ContainSubstring("UUID: GPU-"))
	})
})
