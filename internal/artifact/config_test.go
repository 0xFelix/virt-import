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

	"kubevirt.io/virt-import/internal/artifact"
)

const (
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

var _ = Describe("ValidateConfig", func() {
	const (
		rootDisk = "rootdisk"
		dataDisk = "datadisk"
	)

	DescribeTable("should reject", func(artifactType, config string, disks []string, message string) {
		_, err := artifact.ValidateConfig(artifactType, []byte(config), disks)
		Expect(err).To(MatchError(artifact.ErrInvalidArtifact))
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
		Entry("a DataVolumeTemplate with an invalid source.pvc", artifact.ArtifactTypeVMTemplate,
			templateWithDVT(`{"metadata": {"name": "dv"}, "spec": {"source": {"pvc": "source-rootdisk"}}}`),
			[]string{rootDisk}, `dataVolumeTemplate "dv" has an invalid source.pvc`),
		Entry("a DataVolumeTemplate with an invalid source", artifact.ArtifactTypeVMTemplate,
			templateWithDVT(`{"metadata": {"name": "dv"}, "spec": {"source": "pvc"}}`),
			[]string{rootDisk}, `dataVolumeTemplate "dv" has an invalid source`),
		Entry("an invalid DataVolumeTemplate", artifact.ArtifactTypeVMTemplate, templateWithDVT(`"dv"`),
			[]string{rootDisk}, "invalid dataVolumeTemplate"),
	)

	DescribeTable("should return a valid config unchanged", func(artifactType, config string) {
		obj, err := artifact.ValidateConfig(artifactType, []byte(config), []string{rootDisk, dataDisk})
		Expect(err).ToNot(HaveOccurred())
		expected := map[string]any{}
		Expect(json.Unmarshal([]byte(config), &expected)).To(Succeed())
		Expect(obj.Object).To(Equal(expected))
	},
		Entry("a VirtualMachine", artifact.ArtifactTypeVM, vmConfig),
		Entry("a VirtualMachineTemplate", artifact.ArtifactTypeVMTemplate, templateConfig),
	)
})

// templateWithDVT returns a template whose rootdisk volume uses the DataVolume dv, created by dvt.
func templateWithDVT(dvt string) string {
	return `{
		"apiVersion": "template.kubevirt.io/v1beta1", "kind": "VirtualMachineTemplate",
		"spec": {"virtualMachine": {"spec": {
			"dataVolumeTemplates": [` + dvt + `],
			"template": {"spec": {"volumes": [{"name": "rootdisk", "dataVolume": {"name": "dv"}}]}}
		}}}
	}`
}
