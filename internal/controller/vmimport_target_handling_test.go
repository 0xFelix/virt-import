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

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	virtv1 "kubevirt.io/api/core/v1"

	templatev1beta1 "kubevirt.io/virt-template-api/core/v1beta1"

	"kubevirt.io/virt-import-api/core/v1alpha1"

	"kubevirt.io/virt-import/internal/controller"
	"kubevirt.io/virt-import/internal/testing/artifacts"
)

var _ = Describe("VirtualMachineImport controller target handling", func() {
	var reconciler *controller.VirtualMachineImportReconciler

	BeforeEach(func() {
		reconciler = newReconciler(k8sClient)
	})

	importUntilTarget := func(vmImport *v1alpha1.VirtualMachineImport, fixture string) error {
		completeFetch(reconciler, vmImport, fixtureResult(fixture, ""))
		completeDataVolumes(k8sClient, vmImport)
		_, err := reconcileImport(reconciler, vmImport)
		return err
	}

	It("should create the VirtualMachine", func() {
		vmImport := createImport(k8sClient, testNamespace, withTargetName(testTargetName))
		Expect(importUntilTarget(vmImport, artifacts.VMMultiDisk)).To(Succeed())

		vm := &unstructured.Unstructured{}
		vm.SetGroupVersionKind(virtv1.VirtualMachineGroupVersionKind)
		Expect(k8sClient.Get(context.Background(), client.ObjectKey{Namespace: testNamespace, Name: testTargetName}, vm)).To(Succeed())
		Expect(vm.GetLabels()).To(HaveKeyWithValue(v1alpha1.LabelImportUID, string(vmImport.UID)))
		Expect(vm.GetOwnerReferences()).To(BeEmpty())

		Expect(vmImport.Status.TargetRef).To(Equal(&corev1.ObjectReference{
			APIVersion: virtv1.GroupVersion.String(),
			Kind:       "VirtualMachine",
			Namespace:  testNamespace,
			Name:       testTargetName,
		}))
		expectCondition(vmImport, v1alpha1.ConditionReady, metav1.ConditionTrue, v1alpha1.ReasonImportComplete,
			Equal("VirtualMachine "+testTargetName+" created with 2 disk(s)"))
		expectCondition(vmImport, v1alpha1.ConditionProgressing, metav1.ConditionFalse, v1alpha1.ReasonImportComplete)
	})

	It("should create the VirtualMachineTemplate with the import name", func() {
		vmImport := createImport(k8sClient, testNamespace)
		Expect(importUntilTarget(vmImport, artifacts.VMTemplateDVT)).To(Succeed())

		tpl := &templatev1beta1.VirtualMachineTemplate{}
		Expect(k8sClient.Get(context.Background(), client.ObjectKeyFromObject(vmImport), tpl)).To(Succeed())
		Expect(tpl.Spec.VirtualMachine).ToNot(BeNil())
		Expect(string(tpl.Spec.VirtualMachine.Raw)).To(ContainSubstring(
			`"pvc":{"name":"` + dataVolumeKey(vmImport, artifacts.RootDisk).Name + `","namespace":"` + testNamespace + `"}`,
		))
		Expect(vmImport.Status.TargetRef.Kind).To(Equal("VirtualMachineTemplate"))
		expectCondition(vmImport, v1alpha1.ConditionReady, metav1.ConditionTrue, v1alpha1.ReasonImportComplete)
	})

	It("should fail the import when the target was created meanwhile", func() {
		vmImport := createImport(k8sClient, testNamespace, withTargetName(testTargetName))
		completeFetch(reconciler, vmImport, fixtureResult(artifacts.VMSingle, ""))
		completeDataVolumes(k8sClient, vmImport)

		existing := &unstructured.Unstructured{}
		existing.SetGroupVersionKind(virtv1.VirtualMachineGroupVersionKind)
		existing.SetNamespace(testNamespace)
		existing.SetName(testTargetName)
		Expect(k8sClient.Create(context.Background(), existing)).To(Succeed())

		_, err := reconcileImport(reconciler, vmImport)
		Expect(err).To(MatchError(ContainSubstring("already exists")))
		expectCondition(vmImport, v1alpha1.ConditionProgressing, metav1.ConditionFalse, v1alpha1.ReasonImportFailed)
		Expect(vmImport.Status.TargetRef).To(BeNil())
	})

	It("should complete when its own target exists already", func() {
		vmImport := createImport(k8sClient, testNamespace, withTargetName(testTargetName))
		completeFetch(reconciler, vmImport, fixtureResult(artifacts.VMSingle, ""))
		completeDataVolumes(k8sClient, vmImport)

		// The controller restarted after creating the target, before persisting the status.
		cm := &corev1.ConfigMap{}
		Expect(k8sClient.Get(context.Background(), manifestConfigMapKey(vmImport), cm)).To(Succeed())
		target := &unstructured.Unstructured{}
		Expect(target.UnmarshalJSON([]byte(cm.Data[controller.ManifestKeyTarget]))).To(Succeed())
		Expect(k8sClient.Create(context.Background(), target)).To(Succeed())

		_, err := reconcileImport(reconciler, vmImport)
		Expect(err).ToNot(HaveOccurred())
		expectCondition(vmImport, v1alpha1.ConditionReady, metav1.ConditionTrue, v1alpha1.ReasonImportComplete)
	})
})
