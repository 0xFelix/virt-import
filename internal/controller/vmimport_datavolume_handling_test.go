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
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	cdiv1beta1 "kubevirt.io/containerized-data-importer-api/pkg/apis/core/v1beta1"

	"kubevirt.io/virt-import-api/core/v1alpha1"

	"kubevirt.io/virt-import/internal/artifact"
	"kubevirt.io/virt-import/internal/artifact/artifacttest"
	"kubevirt.io/virt-import/internal/controller"
)

var _ = Describe("VirtualMachineImport controller DataVolume handling", func() {
	const annImmediateBinding = "cdi.kubevirt.io/storage.bind.immediate.requested"

	var reconciler *controller.VirtualMachineImportReconciler

	BeforeEach(func() {
		reconciler = newReconciler(k8sClient)
	})

	It("should create a DataVolume per disk, pinned to the fetched index", func() {
		vmImport := createImport(k8sClient, testNamespace, withTargetName(testTargetName), withRegistryAccess(),
			withStorageClass(testStorageClass))
		result := fixtureResult(artifacttest.VMMultiDisk, "")
		completeFetch(reconciler, vmImport, result)

		for _, disk := range []string{artifacttest.RootDisk, artifacttest.DataDisk} {
			dv := getImportDataVolume(vmImport, disk)
			Expect(dv.Labels).To(HaveKeyWithValue(v1alpha1.LabelImportUID, string(vmImport.UID)))
			Expect(dv.Annotations).To(HaveKey(annImmediateBinding))
			Expect(metav1.IsControlledBy(dv, vmImport)).To(BeTrue())

			registry := dv.Spec.Source.Registry
			Expect(registry.URL).To(HaveValue(Equal("docker://" + testRegistry + "@" + result.Digest.String())))
			Expect(registry.Platform).To(Equal(&cdiv1beta1.PlatformOptions{Architecture: artifacttest.ArchAMD64}))
			Expect(registry.Layer).To(Equal(&cdiv1beta1.LayerSelector{
				MatchAnnotations: map[string]string{artifact.AnnotationDiskName: disk},
			}))
			Expect(registry.SecretRef).To(HaveValue(Equal(testSecret)))
			Expect(registry.CertConfigMap).To(HaveValue(Equal(testCertCM)))
			Expect(registry.PullMethod).To(BeNil())

			Expect(dv.Spec.Storage.StorageClassName).To(HaveValue(Equal(testStorageClass)))
			// The 64Ki disks are below the 1Mi CDI requires at least.
			Expect(dv.Spec.Storage.Resources.Requests.Storage().Equal(resource.MustParse("1Mi"))).To(BeTrue())
			Expect(dv.Spec.Storage.AccessModes).To(BeEmpty())
			Expect(dv.Spec.Storage.VolumeMode).To(BeNil())
		}

		Expect(vmImport.Status.DiskImports).To(ConsistOf(
			v1alpha1.DiskImportStatus{Name: artifacttest.RootDisk, DataVolumeName: dataVolumeKey(vmImport, artifacttest.RootDisk).Name},
			v1alpha1.DiskImportStatus{Name: artifacttest.DataDisk, DataVolumeName: dataVolumeKey(vmImport, artifacttest.DataDisk).Name},
		))
		expectCondition(vmImport, v1alpha1.ConditionProgressing, metav1.ConditionTrue, v1alpha1.ReasonImportingDisks)
		expectCondition(vmImport, v1alpha1.ConditionReady, metav1.ConditionFalse, v1alpha1.ReasonImportingDisks,
			HavePrefix("Waiting for DataVolume "+testNamespace+"/"))
	})

	It("should select the architecture of a multi-arch artifact", func() {
		vmImport := createImport(k8sClient, testNamespace, withArchitecture(artifacttest.ArchARM64))
		completeFetch(reconciler, vmImport, fixtureResult(artifacttest.VMMultiArch, artifacttest.ArchARM64))

		dv := getImportDataVolume(vmImport, artifacttest.RootDisk)
		Expect(dv.Spec.Source.Registry.Platform).To(Equal(&cdiv1beta1.PlatformOptions{Architecture: artifacttest.ArchARM64}))
		Expect(vmImport.Status.DiskImports).To(HaveLen(1))
	})

	It("should pin a plain manifest without a platform", func() {
		vmImport := createImport(k8sClient, testNamespace, withURL("docker://"+testRegistry+"@sha256:"+
			"0000000000000000000000000000000000000000000000000000000000000000"))
		result := fixtureResult(artifacttest.VMPlainManifest, "")
		completeFetch(reconciler, vmImport, result)

		registry := getImportDataVolume(vmImport, artifacttest.RootDisk).Spec.Source.Registry
		Expect(registry.URL).To(HaveValue(Equal("docker://" + testRegistry + "@" + result.ManifestDigest.String())))
		Expect(registry.Platform).To(BeNil())
	})

	It("should not recreate existing DataVolumes", func() {
		vmImport := createImport(k8sClient, testNamespace)
		completeFetch(reconciler, vmImport, fixtureResult(artifacttest.VMSingle, ""))
		uid := getImportDataVolume(vmImport, artifacttest.RootDisk).UID

		_, err := reconcileImport(reconciler, vmImport)
		Expect(err).ToNot(HaveOccurred())
		Expect(getImportDataVolume(vmImport, artifacttest.RootDisk).UID).To(Equal(uid))
	})

	It("should fail the import when a DataVolume does not belong to it", func() {
		vmImport := createImport(k8sClient, testNamespace)
		completeFetch(reconciler, vmImport, fixtureResult(artifacttest.VMSingle, ""))
		dv := getImportDataVolume(vmImport, artifacttest.RootDisk)
		dv.Labels[v1alpha1.LabelImportUID] = wrongUID
		Expect(k8sClient.Update(context.Background(), dv)).To(Succeed())

		_, err := reconcileImport(reconciler, vmImport)
		Expect(err).To(MatchError(ContainSubstring("does not belong to this import")))
		expectCondition(vmImport, v1alpha1.ConditionProgressing, metav1.ConditionFalse, v1alpha1.ReasonImportFailed)
	})

	It("should take over its DataVolumes the cache does not show yet", func() {
		vmImport := createImport(k8sClient, testNamespace)
		completeFetch(reconciler, vmImport, fixtureResult(artifacttest.VMSingle, ""))
		uid := getImportDataVolume(vmImport, artifacttest.RootDisk).UID
		setDataVolumeStatus(k8sClient, getImportDataVolume(vmImport, artifacttest.RootDisk), cdiv1beta1.ImportInProgress, true, "")

		reconciler.Client = hidingClient(&cdiv1beta1.DataVolume{})
		_, err := reconcileImport(reconciler, vmImport)
		Expect(err).ToNot(HaveOccurred())
		Expect(getImportDataVolume(vmImport, artifacttest.RootDisk).UID).To(Equal(uid))
		Expect(vmImport.Status.DiskImports).To(ConsistOf(HaveField("Phase", string(cdiv1beta1.ImportInProgress))))
	})

	It("should fail the import when a foreign DataVolume the cache does not show exists", func() {
		vmImport := createImport(k8sClient, testNamespace)
		foreign := &cdiv1beta1.DataVolume{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: vmImport.Namespace,
				Name:      dataVolumeKey(vmImport, artifacttest.RootDisk).Name,
			},
			Spec: cdiv1beta1.DataVolumeSpec{
				Source: &cdiv1beta1.DataVolumeSource{Registry: &cdiv1beta1.DataVolumeSourceRegistry{URL: ptr.To(testURL)}},
				Storage: &cdiv1beta1.StorageSpec{Resources: corev1.VolumeResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Mi")},
				}},
			},
		}
		Expect(k8sClient.Create(context.Background(), foreign)).To(Succeed())

		reconciler.Client = hidingClient(&cdiv1beta1.DataVolume{})
		_, err := reconcileImport(reconciler, vmImport)
		Expect(err).ToNot(HaveOccurred())
		setJobComplete(k8sClient, fetchJob(k8sClient, vmImport))
		createFetchPod(k8sClient, vmImport, corev1.PodSucceeded, withTerminated(0, ""))
		reconciler.PodLogs = &fakePodLogReader{logs: resultLogs(fixtureResult(artifacttest.VMSingle, ""))}

		_, err = reconcileImport(reconciler, vmImport)
		Expect(err).To(MatchError(ContainSubstring("DataVolume " + foreign.Namespace + "/" + foreign.Name + " does not belong to this import")))
		expectCondition(vmImport, v1alpha1.ConditionProgressing, metav1.ConditionFalse, v1alpha1.ReasonImportFailed)
	})

	It("should fail the import when CDI drops the layer selector", func() {
		watchClient, err := client.NewWithWatch(cfg, client.Options{Scheme: testScheme})
		Expect(err).ToNot(HaveOccurred())
		reconciler.Client = interceptor.NewClient(watchClient, interceptor.Funcs{
			Create: func(ctx context.Context, cli client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if dv, ok := obj.(*cdiv1beta1.DataVolume); ok {
					dv.Spec.Source.Registry.Layer = nil
				}
				return cli.Create(ctx, obj, opts...)
			},
		})
		vmImport := createImport(k8sClient, testNamespace)
		_, err = reconcileImport(reconciler, vmImport)
		Expect(err).ToNot(HaveOccurred())
		setJobComplete(k8sClient, fetchJob(k8sClient, vmImport))
		createFetchPod(k8sClient, vmImport, corev1.PodSucceeded, withTerminated(0, ""))
		reconciler.PodLogs = &fakePodLogReader{logs: resultLogs(fixtureResult(artifacttest.VMSingle, ""))}

		_, err = reconcileImport(reconciler, vmImport)
		Expect(err).To(MatchError(ContainSubstring("does not support layer selection")))
		expectCondition(vmImport, v1alpha1.ConditionProgressing, metav1.ConditionFalse, v1alpha1.ReasonImportFailed)
	})
})

var _ = Describe("VirtualMachineImport controller DataVolume status sync", func() {
	var (
		reconciler *controller.VirtualMachineImportReconciler
		vmImport   *v1alpha1.VirtualMachineImport
		dv         *cdiv1beta1.DataVolume
	)

	BeforeEach(func() {
		reconciler = newReconciler(k8sClient)
		vmImport = createImport(k8sClient, testNamespace)
		completeFetch(reconciler, vmImport, fixtureResult(artifacttest.VMSingle, ""))
		dv = getImportDataVolume(vmImport, artifacttest.RootDisk)
	})

	DescribeTable("should keep waiting for a DataVolume", func(phase cdiv1beta1.DataVolumePhase, running bool) {
		setDataVolumeStatus(k8sClient, dv, phase, running, "")
		_, err := reconcileImport(reconciler, vmImport)
		Expect(err).ToNot(HaveOccurred())

		expectCondition(vmImport, v1alpha1.ConditionProgressing, metav1.ConditionTrue, v1alpha1.ReasonImportingDisks)
		expectCondition(vmImport, v1alpha1.ConditionReady, metav1.ConditionFalse, v1alpha1.ReasonImportingDisks,
			Equal("Waiting for DataVolume "+dv.Namespace+"/"+dv.Name+" to be ready"))
		Expect(vmImport.Status.DiskImports).To(ConsistOf(HaveField("Phase", string(phase))))
	},
		Entry("that is importing", cdiv1beta1.ImportInProgress, true),
		Entry("that is pending", cdiv1beta1.Pending, false),
		Entry("that retries a failed import", cdiv1beta1.ImportScheduled, false),
	)

	It("should report why a DataVolume is not running", func() {
		setDataVolumeStatus(k8sClient, dv, cdiv1beta1.ImportScheduled, false, "Unable to connect to registry data source")
		_, err := reconcileImport(reconciler, vmImport)
		Expect(err).ToNot(HaveOccurred())

		expectCondition(vmImport, v1alpha1.ConditionReady, metav1.ConditionFalse, v1alpha1.ReasonImportingDisks,
			Equal("Waiting for DataVolume "+dv.Namespace+"/"+dv.Name+" to be ready: Unable to connect to registry data source"))
	})

	It("should mirror the DataVolume progress", func() {
		setDataVolumeStatus(k8sClient, dv, cdiv1beta1.ImportInProgress, true, "")
		dv.Status.Progress = "45.3%"
		Expect(k8sClient.Status().Update(context.Background(), dv)).To(Succeed())

		_, err := reconcileImport(reconciler, vmImport)
		Expect(err).ToNot(HaveOccurred())
		Expect(vmImport.Status.DiskImports).To(ConsistOf(HaveField("Progress", "45.3%")))
	})

	It("should fail the import when a DataVolume failed", func() {
		setDataVolumeStatus(k8sClient, dv, cdiv1beta1.Failed, false, "Unable to connect to registry data source")
		_, err := reconcileImport(reconciler, vmImport)
		Expect(err).To(MatchError(ContainSubstring("Unable to connect to registry data source")))

		expectCondition(vmImport, v1alpha1.ConditionProgressing, metav1.ConditionFalse, v1alpha1.ReasonImportFailed)
		expectCondition(vmImport, v1alpha1.ConditionReady, metav1.ConditionFalse, v1alpha1.ReasonImportFailed,
			ContainSubstring("dataVolume "+dv.Namespace+"/"+dv.Name+" failed"))
		Expect(vmImport.Status.DiskImports).To(ConsistOf(HaveField("Phase", string(cdiv1beta1.Failed))))
	})

	It("should move on once all DataVolumes succeeded", func() {
		completeDataVolumes(k8sClient, vmImport)
		_, err := reconcileImport(reconciler, vmImport)
		Expect(err).ToNot(HaveOccurred())

		Expect(vmImport.Status.DiskImports).To(ConsistOf(HaveField("Phase", string(cdiv1beta1.Succeeded))))
		expectCondition(vmImport, v1alpha1.ConditionReady, metav1.ConditionTrue, v1alpha1.ReasonImportComplete)
	})
})
