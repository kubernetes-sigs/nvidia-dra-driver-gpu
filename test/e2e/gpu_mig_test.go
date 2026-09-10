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

// This is the allocation portion of the Bats case "StaticMIG: allocate (1
// cnt)". The Bats case provisions a slice itself; this e2e suite instead
// expects an administrator to have configured static MIG before it starts.
var _ = Describe("static MIG workloads", Label("mig", "static-mig"), func() {
	var ns string

	BeforeEach(func(ctx SpecContext) {
		hasMIG, err := framework.HasDeviceType(ctx, cs, "mig")
		Expect(err).NotTo(HaveOccurred())
		if !hasMIG {
			Skip("no MIG device is advertised by gpu.nvidia.com")
		}

		ns = fmt.Sprintf("gpu-e2e-mig-%d", time.Now().UnixNano()%1_000_000)
		Expect(framework.CreateNamespace(ctx, cs, ns)).To(Succeed())
	})

	AfterEach(func(ctx SpecContext) {
		if ns != "" {
			Expect(cs.CoreV1().Namespaces().Delete(ctx, ns, metav1.DeleteOptions{})).To(Succeed())
		}
	})

	It("allocates one preconfigured MIG device", Label("fastfeedback"), func(ctx SpecContext) {
		yaml, err := framework.Render("gpu-anymig", map[string]any{"Namespace": ns})
		Expect(err).NotTo(HaveOccurred())
		Expect(framework.ApplyYAML(ctx, yaml)).To(Succeed())

		Expect(framework.WaitForPodReady(ctx, cs, ns, "pod-anymig", 3*time.Minute)).To(Succeed())

		logs, err := framework.PodLogs(ctx, cs, ns, "pod-anymig", "ctr")
		Expect(err).NotTo(HaveOccurred())
		Expect(logs).To(ContainSubstring("UUID: MIG-"))
		Expect(logs).To(ContainSubstring("UUID: GPU-"))
	})
})
