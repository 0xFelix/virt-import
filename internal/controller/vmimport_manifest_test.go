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
	"errors"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	virtv1 "kubevirt.io/api/core/v1"

	"kubevirt.io/virt-import-api/core/v1alpha1"

	"kubevirt.io/virt-import/internal/artifact"
	"kubevirt.io/virt-import/internal/controller"
	"kubevirt.io/virt-import/internal/testing/artifacts"
)

var _ = Describe("VirtualMachineImport controller manifest handling", func() {
	var (
		reconciler *controller.VirtualMachineImportReconciler
		vmImport   *v1alpha1.VirtualMachineImport
	)

	BeforeEach(func() {
		reconciler = newReconciler(k8sClient)
		vmImport = createImport(k8sClient, testNamespace, withTargetName(testTargetName))
	})

	manifestConfigMap := func() *corev1.ConfigMap {
		cm := &corev1.ConfigMap{}
		ExpectWithOffset(1, k8sClient.Get(context.Background(), manifestConfigMapKey(vmImport), cm)).To(Succeed())
		return cm
	}

	expectNoManifest := func() {
		err := k8sClient.Get(context.Background(), manifestConfigMapKey(vmImport), &corev1.ConfigMap{})
		ExpectWithOffset(1, err).To(MatchError(k8serrors.IsNotFound, "k8serrors.IsNotFound"))
	}

	// fetchWithLogs runs the metadata fetch with the given fetcher logs and returns the reconcile error.
	fetchWithLogs := func(logs []byte, logErr error) error {
		_, err := reconcileImport(reconciler, vmImport)
		ExpectWithOffset(1, err).ToNot(HaveOccurred())
		setJobComplete(k8sClient, fetchJob(k8sClient, vmImport))
		createFetchPod(k8sClient, vmImport, corev1.PodSucceeded, withTerminated(0, ""))
		reconciler.PodLogs = &fakePodLogReader{logs: logs, err: logErr}
		_, err = reconcileImport(reconciler, vmImport)
		return err
	}

	It("should persist the target and delete the Job", func() {
		result := fixtureResult(artifacts.VMMultiDisk, "")
		completeFetch(reconciler, vmImport, result)

		cm := manifestConfigMap()
		Expect(cm.Labels).To(HaveKeyWithValue(v1alpha1.LabelImportUID, string(vmImport.UID)))
		Expect(metav1.IsControlledBy(cm, vmImport)).To(BeTrue())

		target := &unstructured.Unstructured{}
		Expect(target.UnmarshalJSON([]byte(cm.Data[controller.ManifestKeyTarget]))).To(Succeed())
		Expect(target.GroupVersionKind()).To(Equal(virtv1.VirtualMachineGroupVersionKind))
		Expect(target.GetName()).To(Equal(testTargetName))
		Expect(target.GetNamespace()).To(Equal(testNamespace))
		Expect(target.GetLabels()).To(HaveKeyWithValue(v1alpha1.LabelImportUID, string(vmImport.UID)))
		volumes, _, err := unstructured.NestedSlice(target.Object, "spec", "template", "spec", "volumes")
		Expect(err).ToNot(HaveOccurred())
		Expect(volumes[0]).To(HaveKeyWithValue("persistentVolumeClaim",
			HaveKeyWithValue("claimName", dataVolumeKey(vmImport, artifacts.RootDisk).Name)))
		Expect(volumes[1]).To(HaveKeyWithValue("persistentVolumeClaim",
			HaveKeyWithValue("claimName", dataVolumeKey(vmImport, artifacts.DataDisk).Name)))

		stored := &artifact.FetchResult{}
		Expect(json.Unmarshal([]byte(cm.Data[controller.ManifestKeyResult]), stored)).To(Succeed())
		Expect(stored.Config).To(BeEmpty())
		Expect(stored.Layers).To(Equal(result.Layers))
		Expect(stored.Digest).To(Equal(result.Digest))

		Expect(vmImport.Status.ArtifactInfo).To(Equal(&v1alpha1.ArtifactInfo{
			ArtifactType: artifact.ArtifactTypeVM,
			Architecture: artifacts.ArchAMD64,
			DiskCount:    2,
		}))
		expectNoFetchJob(k8sClient, vmImport)
	})

	It("should fail the import without a result in the logs", func() {
		err := fetchWithLogs([]byte(logLine), nil)
		Expect(err).To(MatchError(ContainSubstring("fetcher output contains no result")))
		expectCondition(vmImport, v1alpha1.ConditionProgressing, metav1.ConditionFalse, v1alpha1.ReasonImportFailed)
		expectNoManifest()
	})

	It("should fail the import with an invalid config", func() {
		err := fetchWithLogs(resultLogs(fixtureResult(artifacts.OrphanLayer, "")), nil)
		Expect(err).To(MatchError(artifact.ErrInvalidArtifact))
		expectCondition(vmImport, v1alpha1.ConditionReady, metav1.ConditionFalse, v1alpha1.ReasonImportFailed,
			ContainSubstring(`disk layer "extradisk" does not name a PVC volume`))
		expectNoManifest()
	})

	It("should retry when the logs cannot be read", func() {
		err := fetchWithLogs(nil, errors.New("connection refused"))
		Expect(err).To(MatchError(ContainSubstring("connection refused")))
		expectCondition(vmImport, v1alpha1.ConditionProgressing, metav1.ConditionTrue, v1alpha1.ReasonFetchingMetadata)
		expectCondition(vmImport, v1alpha1.ConditionReady, metav1.ConditionFalse, v1alpha1.ReasonFetchingMetadata,
			ContainSubstring("connection refused"))
		expectNoManifest()

		reconciler.PodLogs = &fakePodLogReader{logs: resultLogs(fixtureResult(artifacts.VMSingle, ""))}
		_, err = reconcileImport(reconciler, vmImport)
		Expect(err).ToNot(HaveOccurred())
		manifestConfigMap()
	})

	It("should fail the import when the target exists", func() {
		existing := &unstructured.Unstructured{}
		existing.SetGroupVersionKind(virtv1.VirtualMachineGroupVersionKind)
		existing.SetNamespace(testNamespace)
		existing.SetName(testTargetName)
		Expect(k8sClient.Create(context.Background(), existing)).To(Succeed())

		err := fetchWithLogs(resultLogs(fixtureResult(artifacts.VMSingle, "")), nil)
		Expect(err).To(MatchError(ContainSubstring("VirtualMachine " + testNamespace + "/" + testTargetName + " already exists")))
		expectCondition(vmImport, v1alpha1.ConditionProgressing, metav1.ConditionFalse, v1alpha1.ReasonImportFailed)
		expectNoManifest()
	})

	It("should delete a leftover Job when the manifest exists", func() {
		completeFetch(reconciler, vmImport, fixtureResult(artifacts.VMSingle, ""))
		expectNoFetchJob(k8sClient, vmImport)

		// The controller restarted between creating the ConfigMap and deleting the Job.
		leftover := &batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{Namespace: vmImport.Namespace, Name: fetchJobKey(vmImport).Name},
			Spec: batchv1.JobSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
				RestartPolicy: corev1.RestartPolicyNever,
				Containers:    []corev1.Container{{Name: controller.FetcherContainerName, Image: testFetcherImage}},
			}}},
		}
		Expect(k8sClient.Create(context.Background(), leftover)).To(Succeed())

		_, err := reconcileImport(reconciler, vmImport)
		Expect(err).ToNot(HaveOccurred())
		expectNoFetchJob(k8sClient, vmImport)
		manifestConfigMap()
	})

	It("should fail the import when the manifest does not belong to it", func() {
		cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
			Namespace: vmImport.Namespace,
			Name:      manifestConfigMapKey(vmImport).Name,
			Labels:    map[string]string{v1alpha1.LabelImportUID: wrongUID},
		}}
		Expect(k8sClient.Create(context.Background(), cm)).To(Succeed())

		_, err := reconcileImport(reconciler, vmImport)
		Expect(err).To(MatchError(ContainSubstring("does not belong to this import")))
		expectCondition(vmImport, v1alpha1.ConditionProgressing, metav1.ConditionFalse, v1alpha1.ReasonImportFailed)
	})
})
