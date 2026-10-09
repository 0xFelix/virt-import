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

package artifact_test

import (
	"encoding/json"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"

	virtv1 "kubevirt.io/api/core/v1"
	cdiv1beta1 "kubevirt.io/containerized-data-importer-api/pkg/apis/core/v1beta1"

	templatev1beta1 "kubevirt.io/virt-template-api/core/v1beta1"

	"kubevirt.io/virt-import-api/core/v1alpha1"

	"kubevirt.io/virt-import/internal/artifact"
)

var _ = Describe("Rewrite", func() {
	const (
		rootDisk   = "rootdisk"
		dataDisk   = "datadisk"
		targetName = "imported"
		targetNS   = "target-ns"
		importUID  = "import-uid"
		claimRoot  = "claim-rootdisk"
		claimData  = "claim-datadisk"

		vmConfig = `{
			"apiVersion": "kubevirt.io/v1",
			"kind": "VirtualMachine",
			"metadata": {
				"name": "source-vm",
				"uid": "source-uid",
				"resourceVersion": "42",
				"labels": {"app": "demo"}
			},
			"spec": {"template": {"spec": {"volumes": [
				{"name": "rootdisk", "persistentVolumeClaim": {"claimName": "source-rootdisk"}},
				{"name": "datadisk", "persistentVolumeClaim": {"claimName": "source-datadisk", "readOnly": true}},
				{"name": "cloudinit", "cloudInitNoCloud": {"userData": "#cloud-config"}}
			]}}},
			"status": {"ready": true}
		}`

		templateConfig = `{
			"apiVersion": "template.kubevirt.io/v1beta1",
			"kind": "VirtualMachineTemplate",
			"metadata": {"name": "source-template"},
			"spec": {
				"parameters": [{"name": "NAME"}],
				"virtualMachine": {
					"apiVersion": "kubevirt.io/v1",
					"kind": "VirtualMachine",
					"metadata": {"name": "${NAME}"},
					"spec": {
						"dataVolumeTemplates": [
							{"metadata": {"name": "${NAME}-rootdisk"},
							 "spec": {"source": {"pvc": {"name": "source-rootdisk"}}, "storage": {}}},
							{"metadata": {"name": "${NAME}-scratch"},
							 "spec": {"sourceRef": {"kind": "DataSource", "name": "fedora"}, "storage": {}}}
						],
						"template": {"spec": {"volumes": [
							{"name": "rootdisk", "dataVolume": {"name": "${NAME}-rootdisk"}},
							{"name": "scratch", "dataVolume": {"name": "${NAME}-scratch"}},
							{"name": "datadisk", "persistentVolumeClaim": {"claimName": "source-datadisk"}}
						]}}
					}
				}
			}
		}`
	)

	claimName := func(disk string) string {
		return "claim-" + disk
	}

	newOptions := func() *artifact.TargetOptions {
		return &artifact.TargetOptions{
			Name:      targetName,
			Namespace: targetNS,
			ImportUID: importUID,
			ClaimName: claimName,
		}
	}

	pvcVolume := func(claimName string, readOnly bool) *virtv1.PersistentVolumeClaimVolumeSource {
		return &virtv1.PersistentVolumeClaimVolumeSource{
			PersistentVolumeClaimVolumeSource: corev1.PersistentVolumeClaimVolumeSource{ClaimName: claimName, ReadOnly: readOnly},
		}
	}

	It("should rewrite a VirtualMachine", func() {
		obj, err := artifact.Rewrite(artifact.ArtifactTypeVM, []byte(vmConfig), []string{dataDisk, rootDisk}, newOptions())
		Expect(err).ToNot(HaveOccurred())

		// A typed object cannot tell a dropped status from an empty one.
		Expect(obj.Object).ToNot(HaveKey("status"))

		vm := &virtv1.VirtualMachine{}
		Expect(runtime.DefaultUnstructuredConverter.FromUnstructured(obj.Object, vm)).To(Succeed())
		Expect(vm.Name).To(Equal(targetName))
		Expect(vm.Namespace).To(Equal(targetNS))
		Expect(vm.UID).To(BeEmpty())
		Expect(vm.ResourceVersion).To(BeEmpty())
		Expect(vm.Labels).To(Equal(map[string]string{"app": "demo", v1alpha1.LabelImportUID: importUID}))

		volumes := vm.Spec.Template.Spec.Volumes
		Expect(volumes[0].PersistentVolumeClaim).To(Equal(pvcVolume(claimRoot, false)))
		Expect(volumes[1].PersistentVolumeClaim).To(Equal(pvcVolume(claimData, true)))
		Expect(volumes[2].CloudInitNoCloud).To(Equal(&virtv1.CloudInitNoCloudSource{UserData: "#cloud-config"}))
	})

	It("should rewrite a VirtualMachineTemplate", func() {
		obj, err := artifact.Rewrite(artifact.ArtifactTypeVMTemplate, []byte(templateConfig), []string{rootDisk, dataDisk}, newOptions())
		Expect(err).ToNot(HaveOccurred())

		tpl := &templatev1beta1.VirtualMachineTemplate{}
		Expect(runtime.DefaultUnstructuredConverter.FromUnstructured(obj.Object, tpl)).To(Succeed())
		Expect(tpl.Name).To(Equal(targetName))
		Expect(tpl.Namespace).To(Equal(targetNS))

		vm := &virtv1.VirtualMachine{}
		Expect(json.Unmarshal(tpl.Spec.VirtualMachine.Raw, vm)).To(Succeed())
		dvts := vm.Spec.DataVolumeTemplates
		Expect(dvts).To(HaveLen(2))
		Expect(dvts[0].Spec.Source.PVC).To(Equal(&cdiv1beta1.DataVolumeSourcePVC{Name: claimRoot, Namespace: targetNS}))
		Expect(dvts[1].Spec.SourceRef).To(Equal(&cdiv1beta1.DataVolumeSourceRef{Kind: cdiv1beta1.DataVolumeDataSource, Name: "fedora"}))

		volumes := vm.Spec.Template.Spec.Volumes
		Expect(volumes[0].DataVolume).To(Equal(&virtv1.DataVolumeSource{Name: "${NAME}-rootdisk"}))
		Expect(volumes[2].PersistentVolumeClaim).To(Equal(pvcVolume(claimData, false)))
	})

	DescribeTable("should reject", func(artifactType, config string, disks []string, message string) {
		_, err := artifact.Rewrite(artifactType, []byte(config), disks, newOptions())
		Expect(err).To(MatchError(artifact.ErrInvalidArtifact))
		Expect(err).To(MatchError(ContainSubstring(message)))
		_, err = artifact.ValidateConfig(artifactType, []byte(config), disks)
		Expect(err).To(MatchError(ContainSubstring(message)))
	},
		Entry("an unsupported artifactType", "application/vnd.example", vmConfig, nil, "unsupported artifactType"),
		Entry("a config of the wrong kind", artifact.ArtifactTypeVMTemplate, vmConfig, nil, "expected template.kubevirt.io"),
		Entry("a config that cannot be decoded", artifact.ArtifactTypeVM, `{"kind":}`, nil, "cannot be decoded"),
		Entry("a PVC volume without disk", artifact.ArtifactTypeVM, vmConfig, []string{rootDisk},
			`PVC volume "datadisk" is not named by any disk layer`),
		Entry("a disk without PVC volume", artifact.ArtifactTypeVM, vmConfig, []string{rootDisk, dataDisk, "extra"},
			`disk layer "extra" does not name a PVC volume`),
		Entry("a disk naming a volume that does not clone a PVC", artifact.ArtifactTypeVMTemplate, templateConfig,
			[]string{rootDisk, dataDisk, "scratch"}, `disk layer "scratch" does not name a PVC volume`),
		Entry("a DataVolume volume in a VM", artifact.ArtifactTypeVM, `{
			"apiVersion": "kubevirt.io/v1", "kind": "VirtualMachine",
			"spec": {"template": {"spec": {"volumes": [{"name": "rootdisk", "dataVolume": {"name": "dv"}}]}}}
		}`, []string{rootDisk}, "PVC volumes only"),
		Entry("an invalid persistentVolumeClaim", artifact.ArtifactTypeVM, `{
			"apiVersion": "kubevirt.io/v1", "kind": "VirtualMachine",
			"spec": {"template": {"spec": {"volumes": [{"name": "rootdisk", "persistentVolumeClaim": "pvc"}]}}}
		}`, []string{rootDisk}, `volume "rootdisk" has an invalid persistentVolumeClaim`),
	)

	It("should accept a valid config", func() {
		_, err := artifact.ValidateConfig(artifact.ArtifactTypeVM, []byte(vmConfig), []string{rootDisk, dataDisk})
		Expect(err).ToNot(HaveOccurred())
	})
})
