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
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	virtv1 "kubevirt.io/api/core/v1"
	cdiv1beta1 "kubevirt.io/containerized-data-importer-api/pkg/apis/core/v1beta1"

	"kubevirt.io/virt-import-api/core/v1alpha1"

	"kubevirt.io/virt-import/internal/artifact/artifacttest"
	"kubevirt.io/virt-import/internal/controller"
)

var _ = Describe("VirtualMachineImport controller finalizer management and cleanup", func() {
	var reconciler *controller.VirtualMachineImportReconciler

	BeforeEach(func() {
		reconciler = newReconciler(k8sClient)
	})

	expectNotFound := func(obj client.Object) {
		err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(obj), obj)
		ExpectWithOffset(1, err).To(MatchError(k8serrors.IsNotFound, "k8serrors.IsNotFound"))
	}

	// expectControlledByVM expects vm as the only owner of dv, and its controller.
	expectControlledByVM := func(dv *cdiv1beta1.DataVolume, vm *virtv1.VirtualMachine) {
		ExpectWithOffset(1, k8sClient.Get(context.Background(), client.ObjectKeyFromObject(dv), dv)).To(Succeed())
		ExpectWithOffset(1, dv.OwnerReferences).To(ConsistOf(*metav1.NewControllerRef(vm, virtv1.VirtualMachineGroupVersionKind)))
	}

	getTargetVM := func(vmImport *v1alpha1.VirtualMachineImport) *virtv1.VirtualMachine {
		vm := &virtv1.VirtualMachine{}
		ExpectWithOffset(1, k8sClient.Get(context.Background(),
			client.ObjectKey{Namespace: vmImport.Namespace, Name: vmImport.Name}, vm)).To(Succeed())
		return vm
	}

	It("should hand the DataVolumes over to the target once completed and keep the finalizer until deleted", func() {
		vmImport := createImport(k8sClient, testNamespace)
		completeFetch(reconciler, vmImport, fixtureResult(artifacttest.VMSingle, ""))
		dv := getImportDataVolume(vmImport, artifacttest.RootDisk)
		completeDataVolumes(k8sClient, vmImport)
		_, err := reconcileImport(reconciler, vmImport)
		Expect(err).ToNot(HaveOccurred())
		Expect(vmImport.Finalizers).To(ContainElement(v1alpha1.FinalizerImportCleanup))

		_, err = reconcileImport(reconciler, vmImport)
		Expect(err).ToNot(HaveOccurred())
		Expect(vmImport.Finalizers).To(ContainElement(v1alpha1.FinalizerImportCleanup))
		expectNotFound(&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
			Namespace: vmImport.Namespace, Name: manifestConfigMapKey(vmImport).Name,
		}})
		expectControlledByVM(dv, getTargetVM(vmImport))
		expectCondition(vmImport, v1alpha1.ConditionReady, metav1.ConditionTrue, v1alpha1.ReasonImportComplete)

		resourceVersion := vmImport.ResourceVersion
		_, err = reconcileImport(reconciler, vmImport)
		Expect(err).ToNot(HaveOccurred())
		Expect(vmImport.ResourceVersion).To(Equal(resourceVersion), "a completed import must not be written again")

		Expect(k8sClient.Delete(context.Background(), vmImport)).To(Succeed())
		_, err = reconciler.Reconcile(context.Background(), reconcile.Request{NamespacedName: client.ObjectKeyFromObject(vmImport)})
		Expect(err).ToNot(HaveOccurred())
		expectNotFound(vmImport)
		expectControlledByVM(dv, getTargetVM(vmImport))
	})

	It("should delete everything when deleted before completion", func() {
		vmImport := createImport(k8sClient, testNamespace)
		completeFetch(reconciler, vmImport, fixtureResult(artifacttest.VMSingle, ""))
		dv := getImportDataVolume(vmImport, artifacttest.RootDisk)

		Expect(k8sClient.Delete(context.Background(), vmImport)).To(Succeed())
		_, err := reconciler.Reconcile(context.Background(), reconcile.Request{NamespacedName: client.ObjectKeyFromObject(vmImport)})
		Expect(err).ToNot(HaveOccurred())

		expectNotFound(dv)
		expectNoFetchJob(k8sClient, vmImport)
		expectNotFound(vmImport)
	})

	It("should not delete handed-over DataVolumes the cache still shows as its own", func() {
		vmImport := createImport(k8sClient, testNamespace)
		completeFetch(reconciler, vmImport, fixtureResult(artifacttest.VMSingle, ""))
		dv := getImportDataVolume(vmImport, artifacttest.RootDisk)
		completeDataVolumes(k8sClient, vmImport)
		_, err := reconcileImport(reconciler, vmImport)
		Expect(err).ToNot(HaveOccurred())
		stale := &cdiv1beta1.DataVolumeList{}
		Expect(k8sClient.List(context.Background(), stale, client.InNamespace(vmImport.Namespace),
			client.MatchingLabels{v1alpha1.LabelImportUID: string(vmImport.UID)})).To(Succeed())

		_, err = reconcileImport(reconciler, vmImport)
		Expect(err).ToNot(HaveOccurred())
		expectControlledByVM(dv, getTargetVM(vmImport))

		_, err = reconcileImport(newReconciler(staleListClient(stale)), vmImport)
		Expect(err).To(MatchError(k8serrors.IsConflict, "k8serrors.IsConflict"))
		expectControlledByVM(dv, getTargetVM(vmImport))
	})

	It("should hand the DataVolumes over when deleted after creating the target but before completion", func() {
		vmImport := createImport(k8sClient, testNamespace)
		completeFetch(reconciler, vmImport, fixtureResult(artifacttest.VMSingle, ""))
		dv := getImportDataVolume(vmImport, artifacttest.RootDisk)
		vm := createImportedVM(k8sClient, vmImport)

		Expect(k8sClient.Delete(context.Background(), vmImport)).To(Succeed())
		_, err := reconciler.Reconcile(context.Background(), reconcile.Request{NamespacedName: client.ObjectKeyFromObject(vmImport)})
		Expect(err).ToNot(HaveOccurred())

		expectNotFound(vmImport)
		expectControlledByVM(dv, vm)
	})

	It("should delete the DataVolumes when the target is gone before the hand-over", func() {
		vmImport := createImport(k8sClient, testNamespace)
		completeFetch(reconciler, vmImport, fixtureResult(artifacttest.VMSingle, ""))
		dv := getImportDataVolume(vmImport, artifacttest.RootDisk)
		completeDataVolumes(k8sClient, vmImport)
		_, err := reconcileImport(reconciler, vmImport)
		Expect(err).ToNot(HaveOccurred())
		expectCondition(vmImport, v1alpha1.ConditionReady, metav1.ConditionTrue, v1alpha1.ReasonImportComplete)
		Expect(k8sClient.Delete(context.Background(), getTargetVM(vmImport))).To(Succeed())

		_, err = reconcileImport(reconciler, vmImport)
		Expect(err).ToNot(HaveOccurred())
		expectNotFound(dv)
	})

	It("should not hand the DataVolumes over to a target of the same name that belongs to no import", func() {
		vmImport := createImport(k8sClient, testNamespace)
		completeFetch(reconciler, vmImport, fixtureResult(artifacttest.VMSingle, ""))
		dv := getImportDataVolume(vmImport, artifacttest.RootDisk)
		createForeignVM(k8sClient, vmImport.Namespace, vmImport.Name)

		Expect(k8sClient.Delete(context.Background(), vmImport)).To(Succeed())
		_, err := reconciler.Reconcile(context.Background(), reconcile.Request{NamespacedName: client.ObjectKeyFromObject(vmImport)})
		Expect(err).ToNot(HaveOccurred())

		expectNotFound(dv)
		expectNotFound(vmImport)
	})

	It("should delete the metadata-fetch Job when deleted while fetching", func() {
		vmImport := createImport(k8sClient, testNamespace)
		_, err := reconcileImport(reconciler, vmImport)
		Expect(err).ToNot(HaveOccurred())
		fetchJob(k8sClient, vmImport)

		Expect(k8sClient.Delete(context.Background(), vmImport)).To(Succeed())
		_, err = reconciler.Reconcile(context.Background(), reconcile.Request{NamespacedName: client.ObjectKeyFromObject(vmImport)})
		Expect(err).ToNot(HaveOccurred())

		expectNoFetchJob(k8sClient, vmImport)
		expectNotFound(vmImport)
	})

	It("should keep the resources of a failed import until it is deleted", func() {
		vmImport := createImport(k8sClient, testNamespace)
		completeFetch(reconciler, vmImport, fixtureResult(artifacttest.VMSingle, ""))
		dv := getImportDataVolume(vmImport, artifacttest.RootDisk)
		setDataVolumeStatus(k8sClient, dv, cdiv1beta1.Failed, false, "import failed")
		_, err := reconcileImport(reconciler, vmImport)
		Expect(err).To(HaveOccurred())

		_, err = reconcileImport(reconciler, vmImport)
		Expect(err).ToNot(HaveOccurred())
		Expect(vmImport.Finalizers).To(ContainElement(v1alpha1.FinalizerImportCleanup))
		Expect(k8sClient.Get(context.Background(), client.ObjectKeyFromObject(dv), dv)).To(Succeed())

		Expect(k8sClient.Delete(context.Background(), vmImport)).To(Succeed())
		_, err = reconciler.Reconcile(context.Background(), reconcile.Request{NamespacedName: client.ObjectKeyFromObject(vmImport)})
		Expect(err).ToNot(HaveOccurred())
		expectNotFound(dv)
		expectNotFound(vmImport)
	})
})
