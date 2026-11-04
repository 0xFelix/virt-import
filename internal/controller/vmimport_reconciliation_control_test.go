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
	"errors"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	batchv1 "k8s.io/api/batch/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"kubevirt.io/virt-import-api/core/v1alpha1"

	"kubevirt.io/virt-import/internal/controller"
)

var _ = Describe("VirtualMachineImport controller reconciliation control", func() {
	var reconciler *controller.VirtualMachineImportReconciler

	BeforeEach(func() {
		reconciler = newReconciler(k8sClient)
	})

	setConditions := func(vmImport *v1alpha1.VirtualMachineImport, conditions ...metav1.Condition) {
		for _, cond := range conditions {
			meta.SetStatusCondition(&vmImport.Status.Conditions, cond)
		}
		ExpectWithOffset(1, k8sClient.Status().Update(context.Background(), vmImport)).To(Succeed())
	}

	expectNoJob := func(vmImport *v1alpha1.VirtualMachineImport) {
		list := &batchv1.JobList{}
		ExpectWithOffset(1, k8sClient.List(context.Background(), list, client.InNamespace(vmImport.Namespace),
			client.MatchingLabels{v1alpha1.LabelImportUID: string(vmImport.UID)})).To(Succeed())
		ExpectWithOffset(1, list.Items).To(BeEmpty())
	}

	It("should not restart a failed import", func() {
		vmImport := createImport(k8sClient, testNamespace)
		setConditions(vmImport,
			metav1.Condition{Type: v1alpha1.ConditionReady, Status: metav1.ConditionFalse, Reason: v1alpha1.ReasonImportFailed},
			metav1.Condition{Type: v1alpha1.ConditionProgressing, Status: metav1.ConditionFalse, Reason: v1alpha1.ReasonImportFailed},
		)

		_, err := reconcileImport(reconciler, vmImport)
		Expect(err).ToNot(HaveOccurred())
		expectNoJob(vmImport)
		expectCondition(vmImport, v1alpha1.ConditionProgressing, metav1.ConditionFalse, v1alpha1.ReasonImportFailed)
	})

	It("should keep the Progressing transition time while the import is in progress", func() {
		vmImport := createImport(k8sClient, testNamespace)
		_, err := reconcileImport(reconciler, vmImport)
		Expect(err).ToNot(HaveOccurred())
		_, err = reconcileImport(reconciler, vmImport)
		Expect(err).ToNot(HaveOccurred())

		// The API server stores seconds, so an earlier time stands in for a reconcile a second later.
		transitionTime := metav1.NewTime(time.Now().Add(-time.Hour).Truncate(time.Second))
		meta.FindStatusCondition(vmImport.Status.Conditions, v1alpha1.ConditionProgressing).LastTransitionTime = transitionTime
		Expect(k8sClient.Status().Update(context.Background(), vmImport)).To(Succeed())
		resourceVersion := vmImport.ResourceVersion

		_, err = reconcileImport(reconciler, vmImport)
		Expect(err).ToNot(HaveOccurred())
		Expect(vmImport.ResourceVersion).To(Equal(resourceVersion), "an unchanged import must not be written")
		progressing := meta.FindStatusCondition(vmImport.Status.Conditions, v1alpha1.ConditionProgressing)
		Expect(progressing.LastTransitionTime.Equal(&transitionTime)).To(BeTrue())
	})

	It("should start an import that waited for its required CRDs", func() {
		vmImport := createImport(k8sClient, testNamespace)
		setConditions(vmImport,
			metav1.Condition{Type: v1alpha1.ConditionReady, Status: metav1.ConditionFalse, Reason: v1alpha1.ReasonWaiting},
			metav1.Condition{Type: v1alpha1.ConditionProgressing, Status: metav1.ConditionTrue, Reason: v1alpha1.ReasonWaiting},
		)

		_, err := reconcileImport(reconciler, vmImport)
		Expect(err).ToNot(HaveOccurred())
		fetchJob(k8sClient, vmImport)
		expectCondition(vmImport, v1alpha1.ConditionProgressing, metav1.ConditionTrue, v1alpha1.ReasonFetchingMetadata)
		expectCondition(vmImport, v1alpha1.ConditionReady, metav1.ConditionFalse, v1alpha1.ReasonFetchingMetadata)
	})

	DescribeTable("should fail an invalid import", func(opt importOpt, message string) {
		vmImport := createImport(k8sClient, testNamespace, opt)

		_, err := reconcileImport(reconciler, vmImport)
		Expect(err).To(MatchError(ContainSubstring(message)))
		expectNoJob(vmImport)
		expectCondition(vmImport, v1alpha1.ConditionProgressing, metav1.ConditionFalse, v1alpha1.ReasonImportFailed)
		expectCondition(vmImport, v1alpha1.ConditionReady, metav1.ConditionFalse, v1alpha1.ReasonImportFailed,
			ContainSubstring(message))
	},
		Entry("with an invalid reference", withURL("docker://Registry.example.com/UPPER"), "invalid registry url"),
	)

	It("should fail an import without a registry source", func() {
		vmImport := createImport(k8sClient, testNamespace)
		watchClient, err := client.NewWithWatch(cfg, client.Options{Scheme: testScheme})
		Expect(err).ToNot(HaveOccurred())
		dropRegistry := func(obj client.Object) {
			if found, ok := obj.(*v1alpha1.VirtualMachineImport); ok {
				found.Spec.Source.Registry = nil
			}
		}
		// The CRD requires the registry source, so only a client can drop it, also from the
		// object the finalizer patch returns.
		reconciler.Client = interceptor.NewClient(watchClient, interceptor.Funcs{
			Get: func(
				ctx context.Context, cli client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption,
			) error {
				getErr := cli.Get(ctx, key, obj, opts...)
				dropRegistry(obj)
				return getErr
			},
			Patch: func(
				ctx context.Context, cli client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption,
			) error {
				patchErr := cli.Patch(ctx, obj, patch, opts...)
				dropRegistry(obj)
				return patchErr
			},
		})

		_, err = reconcileImport(reconciler, vmImport)
		Expect(err).To(MatchError(ContainSubstring("a registry source is required")))
		expectNoJob(vmImport)
		expectCondition(vmImport, v1alpha1.ConditionProgressing, metav1.ConditionFalse, v1alpha1.ReasonImportFailed)
	})

	It("should retry a failed import whose status cannot be patched", func() {
		vmImport := createImport(k8sClient, testNamespace, withURL("docker://Registry.example.com/UPPER"))
		watchClient, err := client.NewWithWatch(cfg, client.Options{Scheme: testScheme})
		Expect(err).ToNot(HaveOccurred())
		reconciler.Client = interceptor.NewClient(watchClient, interceptor.Funcs{
			SubResourcePatch: func(
				context.Context, client.Client, string, client.Object, client.Patch, ...client.SubResourcePatchOption,
			) error {
				return errors.New("status patch failed")
			},
		})

		_, err = reconcileImport(reconciler, vmImport)
		Expect(err).To(MatchError(ContainSubstring("status patch failed")))
		Expect(err).To(MatchError(ContainSubstring("invalid registry url")))
		Expect(errors.Is(err, reconcile.TerminalError(nil))).To(BeFalse())
		Expect(meta.FindStatusCondition(vmImport.Status.Conditions, v1alpha1.ConditionProgressing)).To(BeNil())
	})

	It("should keep the import name as target name by default", func() {
		vmImport := createImport(k8sClient, testNamespace)
		_, err := reconcileImport(reconciler, vmImport)
		Expect(err).ToNot(HaveOccurred())
		Expect(vmImport.Spec.TargetName).To(BeEmpty())
		fetchJob(k8sClient, vmImport)
	})
})
