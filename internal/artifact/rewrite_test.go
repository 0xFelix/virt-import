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
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

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

		fieldSpec      = "spec"
		fieldClaimName = "claimName"
		fieldName      = "name"
		fieldTemplate  = "template"
		fieldVolumes   = "volumes"

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

	volume := func(obj *unstructured.Unstructured, path []string, i int) map[string]any {
		volumes, found, err := unstructured.NestedSlice(obj.Object, path...)
		ExpectWithOffset(1, err).ToNot(HaveOccurred())
		ExpectWithOffset(1, found).To(BeTrue())
		vol, ok := volumes[i].(map[string]any)
		ExpectWithOffset(1, ok).To(BeTrue())
		return vol
	}

	It("should rewrite a VirtualMachine", func() {
		obj, err := artifact.Rewrite(artifact.ArtifactTypeVM, []byte(vmConfig), []string{dataDisk, rootDisk}, newOptions())
		Expect(err).ToNot(HaveOccurred())

		Expect(obj.GetName()).To(Equal(targetName))
		Expect(obj.GetNamespace()).To(Equal(targetNS))
		Expect(obj.GetUID()).To(BeEmpty())
		Expect(obj.GetResourceVersion()).To(BeEmpty())
		Expect(obj.GetLabels()).To(Equal(map[string]string{"app": "demo", v1alpha1.LabelImportUID: importUID}))
		Expect(obj.Object).ToNot(HaveKey("status"))

		path := []string{fieldSpec, fieldTemplate, fieldSpec, fieldVolumes}
		Expect(volume(obj, path, 0)).To(HaveKeyWithValue("persistentVolumeClaim",
			map[string]any{fieldClaimName: claimRoot}))
		Expect(volume(obj, path, 1)).To(HaveKeyWithValue("persistentVolumeClaim",
			map[string]any{fieldClaimName: claimData, "readOnly": true}))
		Expect(volume(obj, path, 2)).To(HaveKeyWithValue("cloudInitNoCloud",
			map[string]any{"userData": "#cloud-config"}))
	})

	It("should rewrite a VirtualMachineTemplate", func() {
		obj, err := artifact.Rewrite(artifact.ArtifactTypeVMTemplate, []byte(templateConfig), []string{rootDisk, dataDisk}, newOptions())
		Expect(err).ToNot(HaveOccurred())

		Expect(obj.GetName()).To(Equal(targetName))
		Expect(obj.GetNamespace()).To(Equal(targetNS))

		dvts, _, err := unstructured.NestedSlice(obj.Object, fieldSpec, "virtualMachine", fieldSpec, "dataVolumeTemplates")
		Expect(err).ToNot(HaveOccurred())
		Expect(dvts).To(HaveLen(2))
		Expect(dvts[0]).To(HaveKeyWithValue(fieldSpec, HaveKeyWithValue("source",
			HaveKeyWithValue("pvc", map[string]any{fieldName: claimRoot, "namespace": targetNS}))))
		Expect(dvts[1]).To(HaveKeyWithValue(fieldSpec, HaveKeyWithValue("sourceRef",
			map[string]any{"kind": "DataSource", fieldName: "fedora"})))

		path := []string{fieldSpec, "virtualMachine", fieldSpec, fieldTemplate, fieldSpec, fieldVolumes}
		Expect(volume(obj, path, 0)).To(HaveKeyWithValue("dataVolume", map[string]any{fieldName: "${NAME}-rootdisk"}))
		Expect(volume(obj, path, 2)).To(HaveKeyWithValue("persistentVolumeClaim", map[string]any{fieldClaimName: claimData}))
	})

	DescribeTable("should reject", func(artifactType, config string, disks []string, message string) {
		_, err := artifact.Rewrite(artifactType, []byte(config), disks, newOptions())
		Expect(err).To(MatchError(artifact.ErrInvalidArtifact))
		Expect(err).To(MatchError(ContainSubstring(message)))
		Expect(artifact.ValidateConfig(artifactType, []byte(config), disks)).To(MatchError(ContainSubstring(message)))
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
	)

	It("should accept a valid config", func() {
		Expect(artifact.ValidateConfig(artifact.ArtifactTypeVM, []byte(vmConfig), []string{rootDisk, dataDisk})).To(Succeed())
	})
})
