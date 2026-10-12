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

	. "github.com/onsi/gomega"
	gomegatypes "github.com/onsi/gomega/types"

	"github.com/opencontainers/go-digest"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	cdiv1beta1 "kubevirt.io/containerized-data-importer-api/pkg/apis/core/v1beta1"

	"kubevirt.io/virt-import-api/core/v1alpha1"

	"kubevirt.io/virt-import/internal/apimachinery"
	"kubevirt.io/virt-import/internal/artifact"
	"kubevirt.io/virt-import/internal/controller"
	"kubevirt.io/virt-import/internal/testing/artifacts"
)

// This file contains common constants and helpers shared among the vmimport_*_test.go files.

const (
	testRegistry     = "registry.example.com/vms/fedora"
	testURL          = "docker://" + testRegistry + ":v1"
	testFetcherImage = "quay.io/kubevirt/virt-import-fetcher:test"
	testImportPrefix = "test-import-"
	testTargetName   = "imported-vm"
	testSecret       = "registry-credentials" //nolint:gosec // A Secret name, not a credential.
	testCertCM       = "registry-ca"
	testStorageClass = "fast"
	wrongUID         = "wrong-uid"
	logLine          = "time=2026-01-01T00:00:00Z level=INFO msg=\"Fetching artifact metadata\"\n"
)

type fakePodLogReader struct {
	logs []byte
	err  error
}

func (f *fakePodLogReader) GetLogs(_ context.Context, _, _, _ string) ([]byte, error) {
	return f.logs, f.err
}

func newReconciler(cli client.Client) *controller.VirtualMachineImportReconciler {
	return &controller.VirtualMachineImportReconciler{
		Client:       cli,
		Scheme:       cli.Scheme(),
		PodLogs:      &fakePodLogReader{},
		FetcherImage: testFetcherImage,
	}
}

type importOpt func(*v1alpha1.VirtualMachineImport)

func withTargetName(name string) importOpt {
	return func(vmImport *v1alpha1.VirtualMachineImport) {
		vmImport.Spec.TargetName = ptr.To(name)
	}
}

func withArchitecture(arch string) importOpt {
	return func(vmImport *v1alpha1.VirtualMachineImport) {
		vmImport.Spec.Source.Registry.Platform = &v1alpha1.PlatformOptions{Architecture: ptr.To(arch)}
	}
}

func withRegistryAccess() importOpt {
	return func(vmImport *v1alpha1.VirtualMachineImport) {
		vmImport.Spec.Source.Registry.SecretRef = ptr.To(testSecret)
		vmImport.Spec.Source.Registry.CertConfigMap = ptr.To(testCertCM)
	}
}

func withStorageClass(name string) importOpt {
	return func(vmImport *v1alpha1.VirtualMachineImport) {
		vmImport.Spec.StorageClassName = ptr.To(name)
	}
}

func withURL(url string) importOpt {
	return func(vmImport *v1alpha1.VirtualMachineImport) {
		vmImport.Spec.Source.Registry.URL = url
	}
}

func createImport(cli client.Client, namespace string, opts ...importOpt) *v1alpha1.VirtualMachineImport {
	vmImport := &v1alpha1.VirtualMachineImport{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: testImportPrefix,
			Namespace:    namespace,
		},
		Spec: v1alpha1.VirtualMachineImportSpec{
			Source: v1alpha1.VirtualMachineImportSource{
				Registry: &v1alpha1.VirtualMachineImportRegistrySource{URL: testURL},
			},
		},
	}
	for _, opt := range opts {
		opt(vmImport)
	}
	ExpectWithOffset(1, cli.Create(context.Background(), vmImport)).To(Succeed())
	return vmImport
}

// reconcileImport reconciles vmImport and refreshes it afterwards.
func reconcileImport(
	reconciler *controller.VirtualMachineImportReconciler, vmImport *v1alpha1.VirtualMachineImport,
) (reconcile.Result, error) {
	result, err := reconciler.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: client.ObjectKeyFromObject(vmImport),
	})
	ExpectWithOffset(1, reconciler.Get(context.Background(), client.ObjectKeyFromObject(vmImport), vmImport)).To(Succeed())
	return result, err
}

func fetchJobKey(vmImport *v1alpha1.VirtualMachineImport) client.ObjectKey {
	return client.ObjectKey{
		Namespace: vmImport.Namespace,
		Name:      apimachinery.GetStableName(vmImport.Name, string(vmImport.UID), "fetch"),
	}
}

func fetchJob(cli client.Client, vmImport *v1alpha1.VirtualMachineImport) *batchv1.Job {
	job := &batchv1.Job{}
	ExpectWithOffset(1, cli.Get(context.Background(), fetchJobKey(vmImport), job)).To(Succeed())
	return job
}

func expectNoFetchJob(cli client.Client, vmImport *v1alpha1.VirtualMachineImport) {
	err := cli.Get(context.Background(), fetchJobKey(vmImport), &batchv1.Job{})
	ExpectWithOffset(1, err).To(MatchError(k8serrors.IsNotFound, "k8serrors.IsNotFound"))
}

func getImportDataVolume(vmImport *v1alpha1.VirtualMachineImport, disk string) *cdiv1beta1.DataVolume {
	dv := &cdiv1beta1.DataVolume{}
	ExpectWithOffset(1, k8sClient.Get(context.Background(), dataVolumeKey(vmImport, disk), dv)).To(Succeed())
	return dv
}

func manifestConfigMapKey(vmImport *v1alpha1.VirtualMachineImport) client.ObjectKey {
	return client.ObjectKey{
		Namespace: vmImport.Namespace,
		Name:      apimachinery.GetStableName(vmImport.Name, string(vmImport.UID), "manifest"),
	}
}

func dataVolumeKey(vmImport *v1alpha1.VirtualMachineImport, disk string) client.ObjectKey {
	target := vmImport.Name
	if vmImport.Spec.TargetName != nil {
		target = *vmImport.Spec.TargetName
	}
	return client.ObjectKey{
		Namespace: vmImport.Namespace,
		Name:      apimachinery.GetStableName(target, string(vmImport.UID), disk),
	}
}

// setJobComplete marks a Job as complete, with the conditions the API server requires.
func setJobComplete(cli client.Client, job *batchv1.Job) {
	now := metav1.Now()
	job.Status.StartTime = &now
	job.Status.CompletionTime = &now
	job.Status.Succeeded = 1
	job.Status.Conditions = []batchv1.JobCondition{
		{Type: batchv1.JobSuccessCriteriaMet, Status: corev1.ConditionTrue, LastTransitionTime: now},
		{Type: batchv1.JobComplete, Status: corev1.ConditionTrue, LastTransitionTime: now},
	}
	ExpectWithOffset(1, cli.Status().Update(context.Background(), job)).To(Succeed())
}

// setJobFailed marks a Job as failed, with the conditions the API server requires.
func setJobFailed(cli client.Client, job *batchv1.Job, reason, message string) {
	now := metav1.Now()
	job.Status.StartTime = &now
	job.Status.Failed = 1
	job.Status.Conditions = []batchv1.JobCondition{
		{Type: batchv1.JobFailureTarget, Status: corev1.ConditionTrue, Reason: reason, Message: message, LastTransitionTime: now},
		{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Reason: reason, Message: message, LastTransitionTime: now},
	}
	ExpectWithOffset(1, cli.Status().Update(context.Background(), job)).To(Succeed())
}

type podStatusOpt func(*corev1.PodStatus)

func withTerminated(exitCode int32, message string) podStatusOpt {
	return func(status *corev1.PodStatus) {
		status.ContainerStatuses[0].State = corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
			ExitCode:   exitCode,
			Message:    message,
			FinishedAt: metav1.Now(),
		}}
	}
}

func withWaiting(reason, message string) podStatusOpt {
	return func(status *corev1.PodStatus) {
		status.ContainerStatuses[0].State = corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{
			Reason:  reason,
			Message: message,
		}}
	}
}

// createFetchPod creates a Pod of the metadata-fetch Job of vmImport in the given phase.
func createFetchPod(
	cli client.Client, vmImport *v1alpha1.VirtualMachineImport, phase corev1.PodPhase, opts ...podStatusOpt,
) *corev1.Pod {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "fetch-",
			Namespace:    vmImport.Namespace,
			Labels:       map[string]string{v1alpha1.LabelImportUID: string(vmImport.UID)},
		},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			Containers:    []corev1.Container{{Name: controller.FetcherContainerName, Image: testFetcherImage}},
		},
	}
	ExpectWithOffset(1, cli.Create(context.Background(), pod)).To(Succeed())

	pod.Status = corev1.PodStatus{
		Phase:             phase,
		ContainerStatuses: []corev1.ContainerStatus{{Name: controller.FetcherContainerName, Image: testFetcherImage}},
	}
	for _, opt := range opts {
		opt(&pod.Status)
	}
	ExpectWithOffset(1, cli.Status().Update(context.Background(), pod)).To(Succeed())
	return pod
}

// fixtureResult returns the fetch result for the image of a fixture with the given architecture,
// or its first image.
func fixtureResult(name, arch string) *artifact.FetchResult {
	fixture := artifacts.Fixtures()[name]
	img := fixture.Images[0]
	for _, candidate := range fixture.Images {
		if candidate.Architecture == arch {
			img = candidate
		}
	}

	result := &artifact.FetchResult{
		ArtifactType:   img.ArtifactType,
		Architecture:   img.Architecture,
		IsIndex:        !fixture.Plain,
		Digest:         digest.FromString(name + "/index"),
		ManifestDigest: digest.FromString(name + "/" + img.Architecture),
		Config:         json.RawMessage(img.Config),
	}
	if fixture.Plain {
		result.Digest = result.ManifestDigest
	}
	for _, disk := range img.Disks {
		result.Layers = append(result.Layers, artifact.Layer{
			Digest:   digest.FromBytes(disk.Content),
			DiskName: disk.Name,
			DiskSize: "64Ki",
		})
	}
	return result
}

func resultLogs(result *artifact.FetchResult) []byte {
	line, err := json.Marshal(map[string]any{"fetchResult": result})
	ExpectWithOffset(1, err).ToNot(HaveOccurred())
	return append([]byte(logLine), append(line, '\n')...)
}

// completeFetch drives vmImport through the metadata fetch of result: it creates the Job,
// completes it and lets the reconciler read the result.
func completeFetch(
	reconciler *controller.VirtualMachineImportReconciler, vmImport *v1alpha1.VirtualMachineImport,
	result *artifact.FetchResult,
) {
	_, err := reconcileImport(reconciler, vmImport)
	ExpectWithOffset(1, err).ToNot(HaveOccurred())
	setJobComplete(reconciler.Client, fetchJob(reconciler.Client, vmImport))
	createFetchPod(reconciler.Client, vmImport, corev1.PodSucceeded, withTerminated(0, ""))
	reconciler.PodLogs = &fakePodLogReader{logs: resultLogs(result)}
	_, err = reconcileImport(reconciler, vmImport)
	ExpectWithOffset(1, err).ToNot(HaveOccurred())
}

func setDataVolumeStatus(
	cli client.Client, dv *cdiv1beta1.DataVolume, phase cdiv1beta1.DataVolumePhase, running bool, message string,
) {
	runningStatus := corev1.ConditionFalse
	if running {
		runningStatus = corev1.ConditionTrue
	}
	dv.Status = cdiv1beta1.DataVolumeStatus{
		Phase: phase,
		Conditions: []cdiv1beta1.DataVolumeCondition{
			{Type: cdiv1beta1.DataVolumeRunning, Status: runningStatus, Message: message},
		},
	}
	ExpectWithOffset(1, cli.Status().Update(context.Background(), dv)).To(Succeed())
}

// completeDataVolumes marks every DataVolume of vmImport as succeeded.
func completeDataVolumes(cli client.Client, vmImport *v1alpha1.VirtualMachineImport) {
	list := &cdiv1beta1.DataVolumeList{}
	ExpectWithOffset(1, cli.List(context.Background(), list, client.InNamespace(vmImport.Namespace),
		client.MatchingLabels{v1alpha1.LabelImportUID: string(vmImport.UID)})).To(Succeed())
	ExpectWithOffset(1, list.Items).ToNot(BeEmpty())
	for i := range list.Items {
		setDataVolumeStatus(cli, &list.Items[i], cdiv1beta1.Succeeded, false, "")
	}
}

func expectCondition(
	vmImport *v1alpha1.VirtualMachineImport,
	conditionType string, status metav1.ConditionStatus, reason string,
	messageMatchers ...gomegatypes.GomegaMatcher,
) {
	gExpectCondition(Default, vmImport, conditionType, status, reason, messageMatchers...)
}

func gExpectCondition(
	g Gomega,
	vmImport *v1alpha1.VirtualMachineImport,
	conditionType string, status metav1.ConditionStatus, reason string,
	messageMatchers ...gomegatypes.GomegaMatcher,
) {
	cond := meta.FindStatusCondition(vmImport.Status.Conditions, conditionType)
	g.ExpectWithOffset(2, cond).ToNot(BeNil())
	// Satisfy linters, but already ensured above
	if cond != nil {
		g.ExpectWithOffset(2, cond.Status).To(Equal(status))
		g.ExpectWithOffset(2, cond.Reason).To(Equal(reason))
		for _, matcher := range messageMatchers {
			g.ExpectWithOffset(2, cond.Message).To(matcher)
		}
	}
}
