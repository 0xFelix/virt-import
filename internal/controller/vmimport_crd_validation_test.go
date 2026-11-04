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

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"

	"kubevirt.io/virt-import-api/core/v1alpha1"
)

const (
	invalidName    = "Invalid_Name"
	dns1123Message = "a valid DNS-1123 subdomain"
)

var _ = Describe("VirtualMachineImport CRD validation", func() {
	It("should reject spec changes", func() {
		vmImport := createImport(k8sClient, testNamespace)
		vmImport.Spec.Source.Registry.URL = "docker://registry.example.com/other:v1"
		Expect(k8sClient.Update(context.Background(), vmImport)).To(MatchError(ContainSubstring("spec is immutable")))
	})

	DescribeTable("should reject an invalid spec", func(opt importOpt, message string) {
		Expect(k8sClient.Create(context.Background(), newImport(testNamespace, opt))).To(MatchError(ContainSubstring(message)))
	},
		Entry("without a registry source", func(vmImport *v1alpha1.VirtualMachineImport) {
			vmImport.Spec.Source.Registry = nil
		}, "a registry source is required"),
		Entry("with a URL of another scheme", withURL("oci-archive:/tmp/vm.tar"), "url must start with docker://"),
		Entry("with an invalid target name", withTargetName(invalidName), dns1123Message),
		Entry("with an invalid Secret name", func(vmImport *v1alpha1.VirtualMachineImport) {
			vmImport.Spec.Source.Registry.SecretRef = invalidName
		}, dns1123Message),
		Entry("with an invalid ConfigMap name", func(vmImport *v1alpha1.VirtualMachineImport) {
			vmImport.Spec.Source.Registry.CertConfigMap = invalidName
		}, dns1123Message),
		Entry("with an invalid storage class name", withStorageClass(invalidName), dns1123Message),
	)

	It("should reject an empty storage class name", func() {
		// The typed client omits an empty name, kubectl sends it.
		obj, err := runtime.DefaultUnstructuredConverter.ToUnstructured(newImport(testNamespace))
		Expect(err).ToNot(HaveOccurred())
		vmImport := &unstructured.Unstructured{Object: obj}
		vmImport.SetGroupVersionKind(v1alpha1.GroupVersion.WithKind("VirtualMachineImport"))
		Expect(unstructured.SetNestedField(vmImport.Object, "", "spec", "storageClassName")).To(Succeed())
		Expect(k8sClient.Create(context.Background(), vmImport)).To(MatchError(ContainSubstring("spec.storageClassName")))
	})

	It("should allow metadata changes", func() {
		vmImport := createImport(k8sClient, testNamespace)
		vmImport.Labels = map[string]string{"example.com/team": "virt"}
		Expect(k8sClient.Update(context.Background(), vmImport)).To(Succeed())
		Expect(vmImport.Spec.Source.Registry).ToNot(BeNil())
	})
})
