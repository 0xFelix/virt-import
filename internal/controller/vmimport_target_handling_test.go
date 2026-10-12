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
	"encoding/json"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	virtv1 "kubevirt.io/api/core/v1"
	cdiv1beta1 "kubevirt.io/containerized-data-importer-api/pkg/apis/core/v1beta1"

	templatev1beta1 "kubevirt.io/virt-template-api/core/v1beta1"

	"kubevirt.io/virt-import-api/core/v1alpha1"

	"kubevirt.io/virt-import/internal/artifact/artifacttest"
	"kubevirt.io/virt-import/internal/controller"
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
		Expect(importUntilTarget(vmImport, artifacttest.VMMultiDisk)).To(Succeed())

		vm := &virtv1.VirtualMachine{}
		Expect(k8sClient.Get(context.Background(), client.ObjectKey{Namespace: testNamespace, Name: testTargetName}, vm)).To(Succeed())
		Expect(vm.Labels).To(HaveKeyWithValue(v1alpha1.LabelImportUID, string(vmImport.UID)))
		Expect(vm.OwnerReferences).To(BeEmpty())

		Expect(vmImport.Status.TargetRef).To(Equal(&corev1.TypedLocalObjectReference{
			APIGroup: ptr.To(virtv1.GroupVersion.Group),
			Kind:     "VirtualMachine",
			Name:     testTargetName,
		}))
		expectCondition(vmImport, v1alpha1.ConditionReady, metav1.ConditionTrue, v1alpha1.ReasonImportComplete,
			Equal("VirtualMachine "+testTargetName+" created with 2 disk(s)"))
		expectCondition(vmImport, v1alpha1.ConditionProgressing, metav1.ConditionFalse, v1alpha1.ReasonImportComplete)
	})

	It("should create the VirtualMachineTemplate with the import name", func() {
		vmImport := createImport(k8sClient, testNamespace)
		Expect(importUntilTarget(vmImport, artifacttest.VMTemplateDVT)).To(Succeed())

		tpl := &templatev1beta1.VirtualMachineTemplate{}
		Expect(k8sClient.Get(context.Background(), client.ObjectKeyFromObject(vmImport), tpl)).To(Succeed())
		vm := &virtv1.VirtualMachine{}
		Expect(json.Unmarshal(tpl.Spec.VirtualMachine.Raw, vm)).To(Succeed())
		Expect(vm.Spec.DataVolumeTemplates).To(ConsistOf(HaveField("Spec.Source.PVC", Equal(&cdiv1beta1.DataVolumeSourcePVC{
			Name: dataVolumeKey(vmImport, artifacttest.RootDisk).Name, Namespace: testNamespace,
		}))))
		Expect(vmImport.Status.TargetRef.Kind).To(Equal("VirtualMachineTemplate"))
		expectCondition(vmImport, v1alpha1.ConditionReady, metav1.ConditionTrue, v1alpha1.ReasonImportComplete)
	})

	It("should fail the import when the target was created meanwhile", func() {
		vmImport := createImport(k8sClient, testNamespace, withTargetName(testTargetName))
		completeFetch(reconciler, vmImport, fixtureResult(artifacttest.VMSingle, ""))
		completeDataVolumes(k8sClient, vmImport)

		createForeignVM(k8sClient, testNamespace, testTargetName)

		_, err := reconcileImport(reconciler, vmImport)
		Expect(err).To(MatchError(ContainSubstring("already exists")))
		expectCondition(vmImport, v1alpha1.ConditionProgressing, metav1.ConditionFalse, v1alpha1.ReasonImportFailed)
		Expect(vmImport.Status.TargetRef).To(BeNil())
	})

	It("should fail the import when the target is created between the check and the create", func() {
		vmImport := createImport(k8sClient, testNamespace, withTargetName(testTargetName))
		completeFetch(reconciler, vmImport, fixtureResult(artifacttest.VMSingle, ""))
		completeDataVolumes(k8sClient, vmImport)

		watchClient, err := client.NewWithWatch(cfg, client.Options{Scheme: testScheme})
		Expect(err).ToNot(HaveOccurred())
		reconciler.Client = interceptor.NewClient(watchClient, interceptor.Funcs{
			Create: func(ctx context.Context, cli client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if obj.GetName() == testTargetName {
					createForeignVM(cli, testNamespace, testTargetName)
				}
				return cli.Create(ctx, obj, opts...)
			},
		})

		_, err = reconcileImport(reconciler, vmImport)
		Expect(err).To(MatchError(ContainSubstring("already exists")))
		expectCondition(vmImport, v1alpha1.ConditionProgressing, metav1.ConditionFalse, v1alpha1.ReasonImportFailed)
		Expect(vmImport.Status.TargetRef).To(BeNil())
	})

	It("should fail the import when the API of the target is gone", func() {
		vmImport := createImport(k8sClient, testNamespace, withTargetName(testTargetName))
		completeFetch(reconciler, vmImport, fixtureResult(artifacttest.VMSingle, ""))
		completeDataVolumes(k8sClient, vmImport)

		cm := &corev1.ConfigMap{}
		Expect(k8sClient.Get(context.Background(), manifestConfigMapKey(vmImport), cm)).To(Succeed())
		target := &unstructured.Unstructured{}
		Expect(target.UnmarshalJSON([]byte(cm.Data[controller.ManifestKeyTarget]))).To(Succeed())
		target.SetAPIVersion("missing.kubevirt.io/v1")
		targetJSON, err := target.MarshalJSON()
		Expect(err).ToNot(HaveOccurred())
		cm.Data[controller.ManifestKeyTarget] = string(targetJSON)
		Expect(k8sClient.Update(context.Background(), cm)).To(Succeed())

		_, err = reconcileImport(reconciler, vmImport)
		Expect(err).To(MatchError(ContainSubstring("API group missing.kubevirt.io is not available")))
		expectCondition(vmImport, v1alpha1.ConditionProgressing, metav1.ConditionFalse, v1alpha1.ReasonImportFailed)
	})

	It("should complete when its own target exists already", func() {
		vmImport := createImport(k8sClient, testNamespace, withTargetName(testTargetName))
		completeFetch(reconciler, vmImport, fixtureResult(artifacttest.VMSingle, ""))
		completeDataVolumes(k8sClient, vmImport)

		// The controller restarted after creating the target, before persisting the status.
		cm := &corev1.ConfigMap{}
		Expect(k8sClient.Get(context.Background(), manifestConfigMapKey(vmImport), cm)).To(Succeed())
		target := &virtv1.VirtualMachine{}
		Expect(json.Unmarshal([]byte(cm.Data[controller.ManifestKeyTarget]), target)).To(Succeed())
		Expect(k8sClient.Create(context.Background(), target)).To(Succeed())

		_, err := reconcileImport(reconciler, vmImport)
		Expect(err).ToNot(HaveOccurred())
		expectCondition(vmImport, v1alpha1.ConditionReady, metav1.ConditionTrue, v1alpha1.ReasonImportComplete)
	})
})
