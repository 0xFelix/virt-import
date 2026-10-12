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
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	cdiv1beta1 "kubevirt.io/containerized-data-importer-api/pkg/apis/core/v1beta1"

	"kubevirt.io/virt-import-api/core/v1alpha1"

	"kubevirt.io/virt-import/internal/controller"
	"kubevirt.io/virt-import/internal/testing/artifacts"
)

var _ = Describe("VirtualMachineImport controller finalizer management and cleanup", func() {
	var reconciler *controller.VirtualMachineImportReconciler

	BeforeEach(func() {
		reconciler = newReconciler(k8sClient)
	})

	// createImportedPVC creates the PVC CDI would create for dv.
	createImportedPVC := func(dv client.Object) *corev1.PersistentVolumeClaim {
		pvc := &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: dv.GetNamespace(),
				Name:      dv.GetName(),
				OwnerReferences: []metav1.OwnerReference{{
					APIVersion: "cdi.kubevirt.io/v1beta1", Kind: "DataVolume",
					Name: dv.GetName(), UID: dv.GetUID(), Controller: ptr.To(true),
				}},
			},
			Spec: corev1.PersistentVolumeClaimSpec{
				AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
				Resources:   corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("64Ki")}},
			},
		}
		ExpectWithOffset(1, k8sClient.Create(context.Background(), pvc)).To(Succeed())
		return pvc
	}

	expectNotFound := func(obj client.Object) {
		err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(obj), obj)
		ExpectWithOffset(1, err).To(MatchError(k8serrors.IsNotFound, "k8serrors.IsNotFound"))
	}

	It("should hand the PVCs over and remove the finalizer once completed", func() {
		vmImport := createImport(k8sClient, testNamespace)
		completeFetch(reconciler, vmImport, fixtureResult(artifacts.VMSingle, ""))
		dv := getImportDataVolume(vmImport, artifacts.RootDisk)
		pvc := createImportedPVC(dv)
		completeDataVolumes(k8sClient, vmImport)
		_, err := reconcileImport(reconciler, vmImport)
		Expect(err).ToNot(HaveOccurred())
		Expect(vmImport.Finalizers).To(ContainElement(v1alpha1.FinalizerImportCleanup))

		_, err = reconcileImport(reconciler, vmImport)
		Expect(err).ToNot(HaveOccurred())
		Expect(vmImport.Finalizers).ToNot(ContainElement(v1alpha1.FinalizerImportCleanup))
		expectNotFound(dv)
		Expect(k8sClient.Get(context.Background(), client.ObjectKeyFromObject(pvc), pvc)).To(Succeed())
		Expect(pvc.OwnerReferences).To(BeEmpty())
		expectCondition(vmImport, v1alpha1.ConditionReady, metav1.ConditionTrue, v1alpha1.ReasonImportComplete)

		Expect(k8sClient.Delete(context.Background(), vmImport)).To(Succeed())
		expectNotFound(vmImport)
	})

	It("should delete everything when deleted before completion", func() {
		vmImport := createImport(k8sClient, testNamespace)
		completeFetch(reconciler, vmImport, fixtureResult(artifacts.VMSingle, ""))
		dv := getImportDataVolume(vmImport, artifacts.RootDisk)

		Expect(k8sClient.Delete(context.Background(), vmImport)).To(Succeed())
		_, err := reconciler.Reconcile(context.Background(), reconcile.Request{NamespacedName: client.ObjectKeyFromObject(vmImport)})
		Expect(err).ToNot(HaveOccurred())

		expectNotFound(dv)
		expectNoFetchJob(k8sClient, vmImport)
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
		completeFetch(reconciler, vmImport, fixtureResult(artifacts.VMSingle, ""))
		dv := getImportDataVolume(vmImport, artifacts.RootDisk)
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
