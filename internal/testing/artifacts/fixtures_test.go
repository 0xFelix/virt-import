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

package artifacts_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/klauspost/compress/zstd"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"kubevirt.io/virt-import/internal/artifact"
	"kubevirt.io/virt-import/internal/testing/artifacts"
)

var _ = Describe("Fixtures", func() {
	readBlob := func(dir string, d digest.Digest) []byte {
		content, err := os.ReadFile(filepath.Join(dir, "blobs", d.Algorithm().String(), d.Encoded()))
		ExpectWithOffset(1, err).ToNot(HaveOccurred())
		ExpectWithOffset(1, digest.FromBytes(content)).To(Equal(d))
		return content
	}

	readJSON := func(dir string, d digest.Digest, v any) {
		ExpectWithOffset(1, json.Unmarshal(readBlob(dir, d), v)).To(Succeed())
	}

	// resolve reads a fixture the way the fetcher will and returns the result of validating it.
	resolve := func(name, arch string) (*ocispec.Manifest, []byte, string, error) {
		dir := GinkgoT().TempDir()
		ExpectWithOffset(1, artifacts.Fixtures()[name].WriteLayout(dir)).To(Succeed())

		layoutIndex := ocispec.Index{}
		content, err := os.ReadFile(filepath.Join(dir, ocispec.ImageIndexFile))
		ExpectWithOffset(1, err).ToNot(HaveOccurred())
		ExpectWithOffset(1, json.Unmarshal(content, &layoutIndex)).To(Succeed())
		ExpectWithOffset(1, layoutIndex.Manifests).To(HaveLen(1))

		desc := layoutIndex.Manifests[0]
		indexArtifactType := ""
		if desc.MediaType == ocispec.MediaTypeImageIndex {
			index := ocispec.Index{}
			readJSON(dir, desc.Digest, &index)
			indexArtifactType = index.ArtifactType
			if desc, err = artifact.SelectManifest(&index, arch); err != nil {
				return nil, nil, "", err
			}
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
				To(Equal(artifacts.DiskContent(layer.DiskName, artifacts.DiskSize)))
			ExpectWithOffset(1, layer.DiskSize).To(Equal("64Ki"))
		}
		if err = artifact.ValidateConfig(manifest.ArtifactType, config, names); err != nil {
			return manifest, config, "", err
		}
		configArch, err := artifact.ConfigArchitecture(manifest.ArtifactType, config)
		return manifest, config, configArch, err
	}

	DescribeTable("should be valid", func(name, arch, artifactType, expectedArch string, disks int) {
		manifest, _, configArch, err := resolve(name, arch)
		Expect(err).ToNot(HaveOccurred())
		Expect(manifest.ArtifactType).To(Equal(artifactType))
		Expect(manifest.Layers).To(HaveLen(disks))
		Expect(configArch).To(Equal(expectedArch))
	},
		Entry(artifacts.VMSingle, artifacts.VMSingle, "", artifact.ArtifactTypeVM, artifacts.ArchAMD64, 1),
		Entry(artifacts.VMMultiDisk, artifacts.VMMultiDisk, "", artifact.ArtifactTypeVM, artifacts.ArchAMD64, 2),
		Entry(artifacts.VMMultiArch+" amd64", artifacts.VMMultiArch, artifacts.ArchAMD64,
			artifact.ArtifactTypeVM, artifacts.ArchAMD64, 2),
		Entry(artifacts.VMMultiArch+" arm64", artifacts.VMMultiArch, artifacts.ArchARM64,
			artifact.ArtifactTypeVM, artifacts.ArchARM64, 1),
		Entry(artifacts.VMPlainManifest, artifacts.VMPlainManifest, "", artifact.ArtifactTypeVM, artifacts.ArchAMD64, 1),
		Entry(artifacts.VMTemplateDVT, artifacts.VMTemplateDVT, "", artifact.ArtifactTypeVMTemplate, artifacts.ArchAMD64, 1),
	)

	DescribeTable("should be invalid", func(name, message string) {
		_, _, _, err := resolve(name, "")
		Expect(err).To(MatchError(artifact.ErrInvalidArtifact))
		Expect(err).To(MatchError(ContainSubstring(message)))
	},
		Entry(artifacts.VMMultiArch+" without arch", artifacts.VMMultiArch, "several architectures"),
		Entry(artifacts.BadArtifactType, artifacts.BadArtifactType, "unsupported artifactType"),
		Entry(artifacts.MissingSize, artifacts.MissingSize, "no "+artifact.AnnotationDiskSize),
		Entry(artifacts.DuplicateDisk, artifacts.DuplicateDisk, "named by more than one layer"),
		Entry(artifacts.OrphanLayer, artifacts.OrphanLayer, "does not name a PVC volume"),
		Entry(artifacts.UnlayeredPVC, artifacts.UnlayeredPVC, "is not named by any disk layer"),
		Entry(artifacts.UnknownLayerMediaType, artifacts.UnknownLayerMediaType, "unsupported media type"),
	)

	It("should build every fixture reproducibly", func() {
		for name, fixture := range artifacts.Fixtures() {
			first, second := &bytes.Buffer{}, &bytes.Buffer{}
			Expect(fixture.WriteArchive(first)).To(Succeed())
			Expect(fixture.WriteArchive(second)).To(Succeed())
			Expect(first.Bytes()).To(Equal(second.Bytes()), "fixture %s is not reproducible", name)
		}
	})

	It("should match the committed fixtures", func() {
		for name, fixture := range artifacts.Fixtures() {
			buf := &bytes.Buffer{}
			Expect(fixture.WriteArchive(buf)).To(Succeed())
			committed, err := os.ReadFile(filepath.Join("..", "..", "..", "tests", "artifacts", name+".tar"))
			Expect(err).ToNot(HaveOccurred())
			Expect(buf.Bytes()).To(Equal(committed), "tests/artifacts/%s.tar is outdated, run make generate", name)
		}
	})
})

func decompress(content []byte) []byte {
	decoder, err := zstd.NewReader(nil)
	ExpectWithOffset(1, err).ToNot(HaveOccurred())
	defer decoder.Close()
	out, err := decoder.DecodeAll(content, nil)
	ExpectWithOffset(1, err).ToNot(HaveOccurred())
	return out
}
