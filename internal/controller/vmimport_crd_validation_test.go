/*
 * This file is part of the KubeVirt project
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 *
 * Copyright The KubeVirt Authors.
 *
 */

package controller_test

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("VirtualMachineImport CRD validation", func() {
	It("should reject spec changes", func() {
		vmImport := createImport(k8sClient, testNamespace)
		vmImport.Spec.Source.Registry.URL = "docker://registry.example.com/other:v1"
		Expect(k8sClient.Update(context.Background(), vmImport)).To(MatchError(ContainSubstring("spec is immutable")))
	})

	It("should allow metadata changes", func() {
		vmImport := createImport(k8sClient, testNamespace)
		vmImport.Labels = map[string]string{"example.com/team": "virt"}
		Expect(k8sClient.Update(context.Background(), vmImport)).To(Succeed())
		Expect(vmImport.Spec.Source.Registry).ToNot(BeNil())
	})
})
