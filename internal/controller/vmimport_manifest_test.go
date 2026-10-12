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

	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	virtv1 "kubevirt.io/api/core/v1"

	"kubevirt.io/virt-import-api/core/v1alpha1"

	"kubevirt.io/virt-import/internal/artifact"
	"kubevirt.io/virt-import/internal/artifact/artifacttest"
	"kubevirt.io/virt-import/internal/controller"
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

	fetchWithLogs := func(logs []byte, logErr error) error {
		_, err := reconcileImport(reconciler, vmImport)
		ExpectWithOffset(1, err).ToNot(HaveOccurred())
		setJobComplete(k8sClient, fetchJob(k8sClient, vmImport))
		createFetchPod(k8sClient, vmImport, corev1.PodSucceeded, withTerminated(0, ""))
		reconciler.PodLogs = &fakePodLogReader{logs: logs, err: logErr}
		_, err = reconcileImport(reconciler, vmImport)
		return err
	}

	It("should persist the target", func() {
		result := fixtureResult(artifacttest.VMMultiDisk, "")
		completeFetch(reconciler, vmImport, result)

		cm := manifestConfigMap()
		Expect(cm.Labels).To(HaveKeyWithValue(v1alpha1.LabelImportUID, string(vmImport.UID)))
		Expect(metav1.IsControlledBy(cm, vmImport)).To(BeTrue())

		target := &virtv1.VirtualMachine{}
		Expect(json.Unmarshal([]byte(cm.Data[controller.ManifestKeyTarget]), target)).To(Succeed())
		Expect(target.GroupVersionKind()).To(Equal(virtv1.VirtualMachineGroupVersionKind))
		Expect(target.Name).To(Equal(testTargetName))
		Expect(target.Namespace).To(Equal(testNamespace))
		Expect(target.Labels).To(HaveKeyWithValue(v1alpha1.LabelImportUID, string(vmImport.UID)))
		volumes := target.Spec.Template.Spec.Volumes
		Expect(volumes[0].PersistentVolumeClaim.ClaimName).To(Equal(dataVolumeKey(vmImport, artifacttest.RootDisk).Name))
		Expect(volumes[1].PersistentVolumeClaim.ClaimName).To(Equal(dataVolumeKey(vmImport, artifacttest.DataDisk).Name))

		stored := &artifact.FetchResult{}
		Expect(json.Unmarshal([]byte(cm.Data[controller.ManifestKeyResult]), stored)).To(Succeed())
		Expect(stored.Config).To(BeEmpty())
		Expect(stored.Layers).To(Equal(result.Layers))
		Expect(stored.Digest).To(Equal(result.Digest))

		Expect(vmImport.Status.ArtifactInfo).To(Equal(&v1alpha1.ArtifactInfo{
			ArtifactType: artifact.ArtifactTypeVM,
			Architecture: artifacttest.ArchAMD64,
			Digest:       result.Digest.String(),
		}))
	})

	It("should fail the import without a result in the logs", func() {
		err := fetchWithLogs([]byte(logLine), nil)
		Expect(err).To(MatchError(ContainSubstring("fetcher output contains no result")))
		expectCondition(vmImport, v1alpha1.ConditionProgressing, metav1.ConditionFalse, v1alpha1.ReasonImportFailed)
		expectNoManifest()
	})

	It("should fail the import with an invalid config", func() {
		err := fetchWithLogs(resultLogs(fixtureResult(artifacttest.OrphanLayer, "")), nil)
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

		reconciler.PodLogs = &fakePodLogReader{logs: resultLogs(fixtureResult(artifacttest.VMSingle, ""))}
		_, err = reconcileImport(reconciler, vmImport)
		Expect(err).ToNot(HaveOccurred())
		manifestConfigMap()
	})

	expectFetchRestarted := func(reason string) {
		expectCondition(vmImport, v1alpha1.ConditionProgressing, metav1.ConditionTrue, v1alpha1.ReasonFetchingMetadata)
		expectCondition(vmImport, v1alpha1.ConditionReady, metav1.ConditionFalse, v1alpha1.ReasonFetchingMetadata,
			HavePrefix("Fetching artifact metadata from "+testURL+" again"), HaveSuffix(reason))
		expectNoFetchJob(k8sClient, vmImport)
		expectNoManifest()

		_, err := reconcileImport(reconciler, vmImport)
		Expect(err).ToNot(HaveOccurred())
		fetchJob(k8sClient, vmImport)
	}

	It("should fetch again when the succeeded Pod is gone", func() {
		_, err := reconcileImport(reconciler, vmImport)
		Expect(err).ToNot(HaveOccurred())
		setJobComplete(k8sClient, fetchJob(k8sClient, vmImport))

		_, err = reconcileImport(reconciler, vmImport)
		Expect(err).ToNot(HaveOccurred())
		expectFetchRestarted("its succeeded Pod is gone")
	})

	It("should not read the result of a Pod the Job does not control", func() {
		_, err := reconcileImport(reconciler, vmImport)
		Expect(err).ToNot(HaveOccurred())
		setJobComplete(k8sClient, fetchJob(k8sClient, vmImport))
		pod := createFetchPod(k8sClient, vmImport, corev1.PodSucceeded, withTerminated(0, ""))
		pod.OwnerReferences = nil
		Expect(k8sClient.Update(context.Background(), pod)).To(Succeed())
		reconciler.PodLogs = &fakePodLogReader{logs: resultLogs(fixtureResult(artifacttest.VMSingle, ""))}

		_, err = reconcileImport(reconciler, vmImport)
		Expect(err).ToNot(HaveOccurred())
		expectFetchRestarted("its succeeded Pod is gone")
	})

	It("should fetch again when the logs of the succeeded Pod are gone", func() {
		err := fetchWithLogs(nil, k8serrors.NewNotFound(schema.GroupResource{Resource: "pods"}, "fetch"))
		Expect(err).ToNot(HaveOccurred())
		expectFetchRestarted(" is gone")
	})

	It("should take over its ConfigMap the cache does not show yet", func() {
		completeFetch(reconciler, vmImport, fixtureResult(artifacttest.VMSingle, ""))
		uid := manifestConfigMap().UID

		// The cache missed the ConfigMap, so the completed Job is still there.
		reconciler.Client = hidingClient(&corev1.ConfigMap{})

		_, err := reconcileImport(reconciler, vmImport)
		Expect(err).ToNot(HaveOccurred())
		Expect(manifestConfigMap().UID).To(Equal(uid))
		fetchJob(k8sClient, vmImport)
		expectCondition(vmImport, v1alpha1.ConditionProgressing, metav1.ConditionTrue, v1alpha1.ReasonImportingDisks)
	})

	It("should fail the import when the target exists", func() {
		createForeignVM(k8sClient, testNamespace, testTargetName)

		err := fetchWithLogs(resultLogs(fixtureResult(artifacttest.VMSingle, "")), nil)
		Expect(err).To(MatchError(ContainSubstring("VirtualMachine " + testNamespace + "/" + testTargetName + " already exists")))
		expectCondition(vmImport, v1alpha1.ConditionProgressing, metav1.ConditionFalse, v1alpha1.ReasonImportFailed)
		expectNoManifest()
	})

	It("should delete the Job once the manifest is seen", func() {
		completeFetch(reconciler, vmImport, fixtureResult(artifacttest.VMSingle, ""))
		// The reconcile creating the ConfigMap keeps the Job.
		fetchJob(k8sClient, vmImport)

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
