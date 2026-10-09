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

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"kubevirt.io/virt-import/internal/artifact"
)

var _ = Describe("ValidateManifest", func() {
	const (
		rootDisk = "rootdisk"
		dataDisk = "datadisk"
		size     = "10Gi"
	)

	diskLayer := func(name, size string) ocispec.Descriptor {
		annotations := map[string]string{}
		if name != "" {
			annotations[artifact.AnnotationDiskName] = name
		}
		if size != "" {
			annotations[artifact.AnnotationDiskSize] = size
		}
		return ocispec.Descriptor{
			MediaType:   artifact.MediaTypeDiskRawZstd,
			Digest:      digest.FromString(name),
			Annotations: annotations,
		}
	}

	newManifest := func(layers ...ocispec.Descriptor) *ocispec.Manifest {
		return &ocispec.Manifest{
			MediaType:    ocispec.MediaTypeImageManifest,
			ArtifactType: artifact.ArtifactTypeVM,
			Config:       ocispec.Descriptor{MediaType: artifact.MediaTypeVMConfig},
			Layers:       layers,
		}
	}

	It("should accept a VM manifest", func() {
		Expect(artifact.ValidateManifest(newManifest(diskLayer(rootDisk, size), diskLayer(dataDisk, "1M")), artifact.ArtifactTypeVM)).
			To(Succeed())
	})

	It("should accept a VMTemplate manifest without layers", func() {
		manifest := newManifest()
		manifest.ArtifactType = artifact.ArtifactTypeVMTemplate
		manifest.Config.MediaType = artifact.MediaTypeVMTemplateConfig
		Expect(artifact.ValidateManifest(manifest, "")).To(Succeed())
	})

	DescribeTable("should reject", func(modify func(*ocispec.Manifest), indexArtifactType, message string) {
		manifest := newManifest(diskLayer(rootDisk, size))
		modify(manifest)
		err := artifact.ValidateManifest(manifest, indexArtifactType)
		Expect(err).To(MatchError(artifact.ErrInvalidArtifact))
		Expect(err).To(MatchError(ContainSubstring(message)))
	},
		Entry("a docker manifest", func(m *ocispec.Manifest) {
			m.MediaType = "application/vnd.docker.distribution.manifest.v2+json"
		}, "", "unsupported manifest media type"),
		Entry("an unknown artifactType", func(m *ocispec.Manifest) {
			m.ArtifactType = "application/vnd.example"
		}, "", "unsupported artifactType"),
		Entry("a mismatching index artifactType", func(*ocispec.Manifest) {}, artifact.ArtifactTypeVMTemplate,
			"does not match manifest artifactType"),
		Entry("a mismatching config media type", func(m *ocispec.Manifest) {
			m.Config.MediaType = artifact.MediaTypeVMTemplateConfig
		}, "", "config media type"),
		Entry("an unknown layer media type", func(m *ocispec.Manifest) {
			m.Layers[0].MediaType = "application/vnd.kubevirt.persistentstate.tar+zstd"
		}, "", "unsupported media type"),
		Entry("a layer without disk name", func(m *ocispec.Manifest) {
			m.Layers = append(m.Layers, diskLayer("", size))
		}, "", "no "+artifact.AnnotationDiskName),
		Entry("a duplicate disk name", func(m *ocispec.Manifest) {
			m.Layers = append(m.Layers, diskLayer(rootDisk, size))
		}, "", "named by more than one layer"),
		Entry("a layer without disk size", func(m *ocispec.Manifest) {
			m.Layers = append(m.Layers, diskLayer(dataDisk, ""))
		}, "", "no "+artifact.AnnotationDiskSize),
		Entry("an invalid disk size", func(m *ocispec.Manifest) {
			m.Layers = append(m.Layers, diskLayer(dataDisk, "ten"))
		}, "", "invalid "+artifact.AnnotationDiskSize),
		Entry("a zero disk size", func(m *ocispec.Manifest) {
			m.Layers = append(m.Layers, diskLayer(dataDisk, "0"))
		}, "", "invalid "+artifact.AnnotationDiskSize),
	)
})
