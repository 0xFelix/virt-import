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
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	cdiv1beta1 "kubevirt.io/containerized-data-importer-api/pkg/apis/core/v1beta1"

	"kubevirt.io/virt-import-api/core/v1alpha1"

	"kubevirt.io/virt-import/internal/artifact"
	"kubevirt.io/virt-import/internal/controller"
	"kubevirt.io/virt-import/internal/testing/artifacts"
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
		result := fixtureResult(artifacts.VMMultiDisk, "")
		completeFetch(reconciler, vmImport, result)

		for _, disk := range []string{artifacts.RootDisk, artifacts.DataDisk} {
			dv := getImportDataVolume(vmImport, disk)
			Expect(dv.Labels).To(HaveKeyWithValue(v1alpha1.LabelImportUID, string(vmImport.UID)))
			Expect(dv.Annotations).To(HaveKey(annImmediateBinding))
			Expect(metav1.IsControlledBy(dv, vmImport)).To(BeTrue())

			registry := dv.Spec.Source.Registry
			Expect(registry.URL).To(HaveValue(Equal("docker://" + testRegistry + "@" + result.Digest.String())))
			Expect(registry.Platform).To(Equal(&cdiv1beta1.PlatformOptions{Architecture: artifacts.ArchAMD64}))
			Expect(registry.Layer).To(Equal(&cdiv1beta1.LayerSelector{
				MatchAnnotations: map[string]string{artifact.AnnotationDiskName: disk},
			}))
			Expect(registry.SecretRef).To(HaveValue(Equal(testSecret)))
			Expect(registry.CertConfigMap).To(HaveValue(Equal(testCertCM)))
			Expect(registry.PullMethod).To(BeNil())

			Expect(dv.Spec.Storage.StorageClassName).To(HaveValue(Equal(testStorageClass)))
			Expect(dv.Spec.Storage.Resources.Requests.Storage().Equal(resource.MustParse("64Ki"))).To(BeTrue())
			Expect(dv.Spec.Storage.AccessModes).To(BeEmpty())
			Expect(dv.Spec.Storage.VolumeMode).To(BeNil())
		}

		Expect(vmImport.Status.DiskImports).To(ConsistOf(
			v1alpha1.DiskImportStatus{Name: artifacts.RootDisk, DataVolumeName: dataVolumeKey(vmImport, artifacts.RootDisk).Name},
			v1alpha1.DiskImportStatus{Name: artifacts.DataDisk, DataVolumeName: dataVolumeKey(vmImport, artifacts.DataDisk).Name},
		))
		expectCondition(vmImport, v1alpha1.ConditionProgressing, metav1.ConditionTrue, v1alpha1.ReasonImportingDisks)
		expectCondition(vmImport, v1alpha1.ConditionReady, metav1.ConditionFalse, v1alpha1.ReasonImportingDisks,
			HavePrefix("Waiting for DataVolume "+testNamespace+"/"))
	})

	It("should select the architecture of a multi-arch artifact", func() {
		vmImport := createImport(k8sClient, testNamespace, withArchitecture(artifacts.ArchARM64))
		completeFetch(reconciler, vmImport, fixtureResult(artifacts.VMMultiArch, artifacts.ArchARM64))

		dv := getImportDataVolume(vmImport, artifacts.RootDisk)
		Expect(dv.Spec.Source.Registry.Platform).To(Equal(&cdiv1beta1.PlatformOptions{Architecture: artifacts.ArchARM64}))
		Expect(vmImport.Status.DiskImports).To(HaveLen(1))
	})

	It("should pin a plain manifest without a platform", func() {
		vmImport := createImport(k8sClient, testNamespace, withURL("docker://"+testRegistry+"@sha256:"+
			"0000000000000000000000000000000000000000000000000000000000000000"))
		result := fixtureResult(artifacts.VMPlainManifest, "")
		completeFetch(reconciler, vmImport, result)

		registry := getImportDataVolume(vmImport, artifacts.RootDisk).Spec.Source.Registry
		Expect(registry.URL).To(HaveValue(Equal("docker://" + testRegistry + "@" + result.ManifestDigest.String())))
		Expect(registry.Platform).To(BeNil())
	})

	It("should not recreate existing DataVolumes", func() {
		vmImport := createImport(k8sClient, testNamespace)
		completeFetch(reconciler, vmImport, fixtureResult(artifacts.VMSingle, ""))
		uid := getImportDataVolume(vmImport, artifacts.RootDisk).UID

		_, err := reconcileImport(reconciler, vmImport)
		Expect(err).ToNot(HaveOccurred())
		Expect(getImportDataVolume(vmImport, artifacts.RootDisk).UID).To(Equal(uid))
	})

	It("should fail the import when a DataVolume does not belong to it", func() {
		vmImport := createImport(k8sClient, testNamespace)
		completeFetch(reconciler, vmImport, fixtureResult(artifacts.VMSingle, ""))
		dv := getImportDataVolume(vmImport, artifacts.RootDisk)
		dv.Labels[v1alpha1.LabelImportUID] = wrongUID
		Expect(k8sClient.Update(context.Background(), dv)).To(Succeed())

		_, err := reconcileImport(reconciler, vmImport)
		Expect(err).To(MatchError(ContainSubstring("does not belong to this import")))
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
		reconciler.PodLogs = &fakePodLogReader{logs: resultLogs(fixtureResult(artifacts.VMSingle, ""))}

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
		completeFetch(reconciler, vmImport, fixtureResult(artifacts.VMSingle, ""))
		dv = getImportDataVolume(vmImport, artifacts.RootDisk)
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
