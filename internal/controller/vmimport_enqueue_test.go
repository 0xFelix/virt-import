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
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"kubevirt.io/virt-import-api/core/v1alpha1"

	"kubevirt.io/virt-import/internal/controller"
)

const (
	enqueueImportName = "enqueue-import"
	enqueueImportUID  = "enqueue-import-uid"
	enqueueNamespace  = "enqueue-namespace"
)

var _ = Describe("VirtualMachineImport controller EnqueueRequestByUID", func() {
	var reconciler *controller.VirtualMachineImportReconciler

	BeforeEach(func() {
		cli := fake.NewClientBuilder().
			WithScheme(testScheme).
			WithObjects(&v1alpha1.VirtualMachineImport{ObjectMeta: metav1.ObjectMeta{
				Namespace: enqueueNamespace,
				Name:      enqueueImportName,
				UID:       enqueueImportUID,
			}}).
			WithIndex(&v1alpha1.VirtualMachineImport{}, "metadata.uid", func(obj client.Object) []string {
				return []string{string(obj.GetUID())}
			}).
			Build()
		reconciler = newReconciler(cli)
	})

	DescribeTable("should map a Pod", func(labels map[string]string, expected []reconcile.Request) {
		pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: enqueueNamespace, Name: "fetch", Labels: labels}}
		Expect(reconciler.EnqueueRequestByUID(context.Background(), pod)).To(Equal(expected))
	},
		Entry("with the import UID label to its import", map[string]string{v1alpha1.LabelImportUID: enqueueImportUID},
			[]reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: enqueueNamespace, Name: enqueueImportName}}}),
		Entry("without the label to nothing", map[string]string{}, nil),
		Entry("with an unknown UID to nothing", map[string]string{v1alpha1.LabelImportUID: wrongUID}, nil),
	)
})
