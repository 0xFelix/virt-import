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

package artifacttest_test

import (
	"encoding/json"
	"os"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/klauspost/compress/zstd"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"kubevirt.io/virt-import/internal/artifact"
	"kubevirt.io/virt-import/internal/artifact/artifacttest"
)

var _ = Describe("Fixtures", func() {
	readBlob := func(dir string, d digest.Digest) []byte {
		content, err := os.ReadFile(filepath.Join(dir, "blobs", d.Algorithm().String(), d.Encoded()))
		ExpectWithOffset(1, err).ToNot(HaveOccurred())
		ExpectWithOffset(1, d.Algorithm().FromBytes(content)).To(Equal(d))
		return content
	}

	readJSON := func(dir string, d digest.Digest, v any) {
		ExpectWithOffset(1, json.Unmarshal(readBlob(dir, d), v)).To(Succeed())
	}

	resolve := func(name, arch string) (*ocispec.Manifest, []byte, string, error) {
		dir := GinkgoT().TempDir()
		ExpectWithOffset(1, artifacttest.Fixtures()[name].WriteLayout(dir)).To(Succeed())

		layoutIndex := ocispec.Index{}
		content, err := os.ReadFile(filepath.Join(dir, ocispec.ImageIndexFile))
		ExpectWithOffset(1, err).ToNot(HaveOccurred())
		ExpectWithOffset(1, json.Unmarshal(content, &layoutIndex)).To(Succeed())
		ExpectWithOffset(1, layoutIndex.Manifests).To(HaveLen(1))

		desc := layoutIndex.Manifests[0]
		indexArtifactType, indexArch := "", ""
		if desc.MediaType == ocispec.MediaTypeImageIndex {
			index := ocispec.Index{}
			readJSON(dir, desc.Digest, &index)
			indexArtifactType = index.ArtifactType
			if desc, err = artifact.SelectManifest(&index, arch); err != nil {
				return nil, nil, "", err
			}
			indexArch = desc.Platform.Architecture
		}

		manifest := &ocispec.Manifest{}
		readJSON(dir, desc.Digest, manifest)
		if err = artifact.ValidateManifest(manifest, indexArtifactType); err != nil {
			return manifest, nil, "", err
		}
		config := readBlob(dir, manifest.Config.Digest)
		layers := artifact.LayersFromManifest(manifest)
		names := make([]string, 0, len(layers))
		for _, layer := range layers {
			names = append(names, layer.DiskName)
			ExpectWithOffset(1, decompress(readBlob(dir, layer.Digest))).
				To(Equal(artifacttest.DiskContent(layer.DiskName, artifacttest.DiskSize)))
			ExpectWithOffset(1, layer.DiskSize).To(Equal("64Ki"))
		}
		obj, err := artifact.ValidateConfig(manifest.ArtifactType, config, names)
		if err != nil {
			return manifest, config, "", err
		}
		resolvedArch, err := artifact.ResolveArchitecture(manifest.ArtifactType, obj, indexArch)
		return manifest, config, resolvedArch, err
	}

	DescribeTable("should be valid", func(name, arch, artifactType, expectedArch string, disks int) {
		manifest, _, resolvedArch, err := resolve(name, arch)
		Expect(err).ToNot(HaveOccurred())
		Expect(manifest.ArtifactType).To(Equal(artifactType))
		Expect(manifest.Layers).To(HaveLen(disks))
		Expect(resolvedArch).To(Equal(expectedArch))
	},
		Entry(artifacttest.VMSingle, artifacttest.VMSingle, "", artifact.ArtifactTypeVM, artifacttest.ArchAMD64, 1),
		Entry(artifacttest.VMMultiDisk, artifacttest.VMMultiDisk, "", artifact.ArtifactTypeVM, artifacttest.ArchAMD64, 2),
		Entry(artifacttest.VMMultiArch+" amd64", artifacttest.VMMultiArch, artifacttest.ArchAMD64,
			artifact.ArtifactTypeVM, artifacttest.ArchAMD64, 2),
		Entry(artifacttest.VMMultiArch+" arm64", artifacttest.VMMultiArch, artifacttest.ArchARM64,
			artifact.ArtifactTypeVM, artifacttest.ArchARM64, 1),
		Entry(artifacttest.VMPlainManifest, artifacttest.VMPlainManifest, "", artifact.ArtifactTypeVM, artifacttest.ArchAMD64, 1),
		Entry(artifacttest.VMTemplateDVT, artifacttest.VMTemplateDVT, "", artifact.ArtifactTypeVMTemplate, artifacttest.ArchAMD64, 1),
		Entry(artifacttest.VMUnsetArch, artifacttest.VMUnsetArch, "", artifact.ArtifactTypeVM, artifacttest.ArchARM64, 1),
	)

	DescribeTable("should be invalid", func(name, message string) {
		_, _, _, err := resolve(name, "")
		Expect(err).To(MatchError(artifact.ErrInvalidArtifact))
		Expect(err).To(MatchError(ContainSubstring(message)))
	},
		Entry(artifacttest.VMMultiArch+" without arch", artifacttest.VMMultiArch, "several architectures"),
		Entry(artifacttest.ArchMismatch, artifacttest.ArchMismatch, "does not match config architecture"),
		Entry(artifacttest.BadArtifactType, artifacttest.BadArtifactType, "unsupported artifactType"),
		Entry(artifacttest.MissingSize, artifacttest.MissingSize, "no "+artifact.AnnotationDiskSize),
		Entry(artifacttest.DuplicateDisk, artifacttest.DuplicateDisk, "named by more than one layer"),
		Entry(artifacttest.OrphanLayer, artifacttest.OrphanLayer, "does not name a PVC volume"),
		Entry(artifacttest.UnlayeredPVC, artifacttest.UnlayeredPVC, "is not named by any disk layer"),
		Entry(artifacttest.UnknownLayerMediaType, artifacttest.UnknownLayerMediaType, "unsupported media type"),
	)
})

func decompress(content []byte) []byte {
	decoder, err := zstd.NewReader(nil)
	ExpectWithOffset(1, err).ToNot(HaveOccurred())
	defer decoder.Close()
	out, err := decoder.DecodeAll(content, nil)
	ExpectWithOffset(1, err).ToNot(HaveOccurred())
	return out
}
