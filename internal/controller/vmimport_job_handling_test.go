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

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cdiv1beta1 "kubevirt.io/containerized-data-importer-api/pkg/apis/core/v1beta1"

	"kubevirt.io/virt-import-api/core/v1alpha1"

	"kubevirt.io/virt-import/internal/artifact"
	"kubevirt.io/virt-import/internal/controller"
)

var _ = Describe("VirtualMachineImport controller metadata-fetch Job handling", func() {
	const (
		archARM64       = "arm64"
		certsMountPath  = "/etc/virt-import/certs"
		insecureFlag    = "--insecure-tls"
		terminationText = "invalid artifact: unsupported artifactType"
	)

	var reconciler *controller.VirtualMachineImportReconciler

	BeforeEach(func() {
		reconciler = newReconciler(k8sClient)
	})

	It("should create the metadata-fetch Job", func() {
		vmImport := createImport(k8sClient, testNamespace)
		_, err := reconcileImport(reconciler, vmImport)
		Expect(err).ToNot(HaveOccurred())

		job := fetchJob(k8sClient, vmImport)
		Expect(job.Labels).To(HaveKeyWithValue(v1alpha1.LabelImportUID, string(vmImport.UID)))
		Expect(metav1.IsControlledBy(job, vmImport)).To(BeTrue())
		Expect(job.Spec.BackoffLimit).To(HaveValue(BeEquivalentTo(3)))
		Expect(job.Spec.ActiveDeadlineSeconds).ToNot(BeNil())
		Expect(job.Spec.PodFailurePolicy.Rules).To(ContainElement(HaveField("OnExitCodes.Values",
			ConsistOf(int32(artifact.ExitCodeInvalidArtifact)))))

		podSpec := job.Spec.Template.Spec
		Expect(job.Spec.Template.Labels).To(HaveKeyWithValue(v1alpha1.LabelImportUID, string(vmImport.UID)))
		Expect(podSpec.RestartPolicy).To(Equal(corev1.RestartPolicyNever))
		Expect(podSpec.AutomountServiceAccountToken).To(HaveValue(BeFalse()))
		Expect(podSpec.Containers).To(HaveLen(1))
		container := podSpec.Containers[0]
		Expect(container.Name).To(Equal(controller.FetcherContainerName))
		Expect(container.Image).To(Equal(testFetcherImage))
		Expect(container.Args).To(Equal([]string{"--url=" + testURL}))
		Expect(container.Env).To(BeEmpty())
		Expect(container.SecurityContext.ReadOnlyRootFilesystem).To(HaveValue(BeTrue()))

		expectCondition(vmImport, v1alpha1.ConditionProgressing, metav1.ConditionTrue, v1alpha1.ReasonFetchingMetadata)
		expectCondition(vmImport, v1alpha1.ConditionReady, metav1.ConditionFalse, v1alpha1.ReasonFetchingMetadata,
			Equal("Fetching artifact metadata from "+testURL))
		Expect(vmImport.Finalizers).To(ContainElement(v1alpha1.FinalizerImportCleanup))
	})

	It("should pass architecture, credentials and certificates to the fetcher", func() {
		vmImport := createImport(k8sClient, testNamespace, withArchitecture(archARM64), withRegistryAccess())
		_, err := reconcileImport(reconciler, vmImport)
		Expect(err).ToNot(HaveOccurred())

		podSpec := fetchJob(k8sClient, vmImport).Spec.Template.Spec
		container := podSpec.Containers[0]
		Expect(container.Args).To(ConsistOf("--url="+testURL, "--arch="+archARM64, "--cert-dir="+certsMountPath))
		Expect(container.Env).To(ConsistOf(
			HaveField("ValueFrom.SecretKeyRef", HaveField("Key", "accessKeyId")),
			HaveField("ValueFrom.SecretKeyRef", HaveField("Key", "secretKey")),
		))
		for _, env := range container.Env {
			Expect(env.ValueFrom.SecretKeyRef.Name).To(Equal(testSecret))
		}
		Expect(container.VolumeMounts).To(ContainElement(HaveField("MountPath", certsMountPath)))
		Expect(podSpec.Volumes).To(ContainElement(HaveField("ConfigMap.Name", testCertCM)))
	})

	DescribeTable("should follow the insecure registries of CDI", func(insecureRegistries []string, expectInsecure bool) {
		cdiConfig := &cdiv1beta1.CDIConfig{ObjectMeta: metav1.ObjectMeta{Name: "config"}}
		Expect(k8sClient.Create(context.Background(), cdiConfig)).To(Succeed())
		DeferCleanup(func() {
			Expect(k8sClient.Delete(context.Background(), cdiConfig)).To(Succeed())
		})
		cdiConfig.Spec.InsecureRegistries = insecureRegistries
		Expect(k8sClient.Update(context.Background(), cdiConfig)).To(Succeed())

		vmImport := createImport(k8sClient, testNamespace)
		_, err := reconcileImport(reconciler, vmImport)
		Expect(err).ToNot(HaveOccurred())

		args := fetchJob(k8sClient, vmImport).Spec.Template.Spec.Containers[0].Args
		if expectInsecure {
			Expect(args).To(ContainElement(insecureFlag))
		} else {
			Expect(args).ToNot(ContainElement(insecureFlag))
		}
	},
		Entry("with the registry host listed", []string{"registry.example.com"}, true),
		Entry("with other registries listed", []string{"registry:5000"}, false),
	)

	It("should report a fetcher that cannot start", func() {
		vmImport := createImport(k8sClient, testNamespace, withRegistryAccess())
		_, err := reconcileImport(reconciler, vmImport)
		Expect(err).ToNot(HaveOccurred())
		createFetchPod(k8sClient, vmImport, corev1.PodPending,
			withWaiting("CreateContainerConfigError", `secret "registry-credentials" not found`))

		_, err = reconcileImport(reconciler, vmImport)
		Expect(err).ToNot(HaveOccurred())
		expectCondition(vmImport, v1alpha1.ConditionProgressing, metav1.ConditionTrue, v1alpha1.ReasonFetchingMetadata)
		expectCondition(vmImport, v1alpha1.ConditionReady, metav1.ConditionFalse, v1alpha1.ReasonFetchingMetadata,
			ContainSubstring(`cannot start: CreateContainerConfigError: secret "registry-credentials" not found`))
	})

	It("should report a retried fetch failure", func() {
		vmImport := createImport(k8sClient, testNamespace)
		_, err := reconcileImport(reconciler, vmImport)
		Expect(err).ToNot(HaveOccurred())
		createFetchPod(k8sClient, vmImport, corev1.PodFailed, withTerminated(1, "registry unreachable"))

		_, err = reconcileImport(reconciler, vmImport)
		Expect(err).ToNot(HaveOccurred())
		expectCondition(vmImport, v1alpha1.ConditionProgressing, metav1.ConditionTrue, v1alpha1.ReasonFetchingMetadata)
		expectCondition(vmImport, v1alpha1.ConditionReady, metav1.ConditionFalse, v1alpha1.ReasonFetchingMetadata,
			HaveSuffix("retrying after: registry unreachable"))
	})

	DescribeTable("should fail the import when the Job failed", func(podMessage, expected string) {
		vmImport := createImport(k8sClient, testNamespace)
		_, err := reconcileImport(reconciler, vmImport)
		Expect(err).ToNot(HaveOccurred())
		if podMessage != "" {
			createFetchPod(k8sClient, vmImport, corev1.PodFailed, withTerminated(2, podMessage))
		}
		setJobFailed(k8sClient, fetchJob(k8sClient, vmImport), "DeadlineExceeded", "Job was active longer than specified deadline")

		_, err = reconcileImport(reconciler, vmImport)
		Expect(err).To(MatchError(ContainSubstring(expected)))
		expectCondition(vmImport, v1alpha1.ConditionProgressing, metav1.ConditionFalse, v1alpha1.ReasonImportFailed)
		expectCondition(vmImport, v1alpha1.ConditionReady, metav1.ConditionFalse, v1alpha1.ReasonImportFailed,
			Equal("fetching artifact metadata failed: "+expected))
	},
		Entry("with the fetcher's termination message", terminationText, terminationText),
		Entry("with the Job's reason without one", "", "Job was active longer than specified deadline"),
	)

	It("should fail the import when the Job does not belong to it", func() {
		vmImport := createImport(k8sClient, testNamespace)
		_, err := reconcileImport(reconciler, vmImport)
		Expect(err).ToNot(HaveOccurred())

		job := fetchJob(k8sClient, vmImport)
		job.Labels[v1alpha1.LabelImportUID] = wrongUID
		Expect(k8sClient.Update(context.Background(), job)).To(Succeed())

		_, err = reconcileImport(reconciler, vmImport)
		Expect(err).To(MatchError(ContainSubstring("does not belong to this import")))
		expectCondition(vmImport, v1alpha1.ConditionProgressing, metav1.ConditionFalse, v1alpha1.ReasonImportFailed)
	})

	It("should not recreate the Job while it runs", func() {
		vmImport := createImport(k8sClient, testNamespace)
		_, err := reconcileImport(reconciler, vmImport)
		Expect(err).ToNot(HaveOccurred())
		uid := fetchJob(k8sClient, vmImport).UID

		_, err = reconcileImport(reconciler, vmImport)
		Expect(err).ToNot(HaveOccurred())
		Expect(fetchJob(k8sClient, vmImport).UID).To(Equal(uid))

		list := &batchv1.JobList{}
		Expect(k8sClient.List(context.Background(), list, client.InNamespace(testNamespace),
			client.MatchingLabels{v1alpha1.LabelImportUID: string(vmImport.UID)})).To(Succeed())
		Expect(list.Items).To(HaveLen(1))
		Expect(list.Items[0].Spec.Template.Spec.Containers[0].Image).To(Equal(testFetcherImage))
		Expect(ptr.Deref(list.Items[0].Spec.Template.Spec.EnableServiceLinks, true)).To(BeFalse())
	})
})
