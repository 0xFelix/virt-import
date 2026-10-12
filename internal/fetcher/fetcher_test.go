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

package fetcher_test

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/opencontainers/go-digest"
	"go.podman.io/image/v5/oci/layout"
	"go.podman.io/image/v5/types"

	"kubevirt.io/virt-import/internal/artifact"
	"kubevirt.io/virt-import/internal/artifact/artifacttest"
	"kubevirt.io/virt-import/internal/fetcher"
)

var _ = Describe("Fetch", func() {
	fetchArtifact := func(a *artifacttest.Artifact, arch string) (*artifact.FetchResult, error) {
		dir := GinkgoT().TempDir()
		ExpectWithOffset(2, a.WriteLayout(dir)).To(Succeed())
		ref, err := layout.NewReference(dir, "")
		ExpectWithOffset(2, err).ToNot(HaveOccurred())
		return fetcher.Fetch(context.Background(), ref, &types.SystemContext{}, arch)
	}

	fetch := func(name, arch string) (*artifact.FetchResult, error) {
		return fetchArtifact(artifacttest.Fixtures()[name], arch)
	}

	DescribeTable("should fetch", func(name, arch string, isIndex bool, artifactType, expectedArch string, disks ...string) {
		result, err := fetch(name, arch)
		Expect(err).ToNot(HaveOccurred())
		Expect(result.IsIndex).To(Equal(isIndex))
		Expect(result.ArtifactType).To(Equal(artifactType))
		Expect(result.Architecture).To(Equal(expectedArch))
		Expect(result.DiskNames()).To(Equal(disks))
		Expect(result.Config).ToNot(BeEmpty())
		Expect(result.Digest.Validate()).To(Succeed())
		Expect(result.ManifestDigest.Validate()).To(Succeed())
		if isIndex {
			Expect(result.Digest).ToNot(Equal(result.ManifestDigest))
		} else {
			Expect(result.Digest).To(Equal(result.ManifestDigest))
		}
		for _, layer := range result.Layers {
			Expect(layer.DiskSize).To(Equal("64Ki"))
			Expect(layer.Digest.Validate()).To(Succeed())
		}
	},
		Entry("a single disk VM", artifacttest.VMSingle, "", true, artifact.ArtifactTypeVM, artifacttest.ArchAMD64,
			artifacttest.RootDisk),
		Entry("a multi disk VM", artifacttest.VMMultiDisk, "", true, artifact.ArtifactTypeVM, artifacttest.ArchAMD64,
			artifacttest.RootDisk, artifacttest.DataDisk),
		Entry("amd64 of a multi-arch VM", artifacttest.VMMultiArch, artifacttest.ArchAMD64, true, artifact.ArtifactTypeVM,
			artifacttest.ArchAMD64, artifacttest.RootDisk, artifacttest.DataDisk),
		Entry("arm64 of a multi-arch VM", artifacttest.VMMultiArch, artifacttest.ArchARM64, true, artifact.ArtifactTypeVM,
			artifacttest.ArchARM64, artifacttest.RootDisk),
		Entry("a plain manifest", artifacttest.VMPlainManifest, "", false, artifact.ArtifactTypeVM, artifacttest.ArchAMD64,
			artifacttest.RootDisk),
		Entry("a plain manifest with matching arch", artifacttest.VMPlainManifest, artifacttest.ArchAMD64, false,
			artifact.ArtifactTypeVM, artifacttest.ArchAMD64, artifacttest.RootDisk),
		Entry("a VMTemplate", artifacttest.VMTemplateDVT, "", true, artifact.ArtifactTypeVMTemplate, artifacttest.ArchAMD64,
			artifacttest.RootDisk),
		Entry("the index architecture of a VM without architecture", artifacttest.VMUnsetArch, "", true,
			artifact.ArtifactTypeVM, artifacttest.ArchARM64, artifacttest.RootDisk),
	)

	It("should fetch an artifact with sha512 digests", func() {
		a := artifacttest.Fixtures()[artifacttest.VMSingle]
		a.Algorithm = digest.SHA512
		result, err := fetchArtifact(a, "")
		Expect(err).ToNot(HaveOccurred())
		Expect(result.ManifestDigest.Algorithm()).To(Equal(digest.SHA512))
		Expect(result.Layers).To(HaveLen(1))
		Expect(result.Layers[0].Digest.Algorithm()).To(Equal(digest.SHA512))
	})

	DescribeTable("should reject", func(name, arch, message string) {
		_, err := fetch(name, arch)
		Expect(err).To(MatchError(artifact.ErrInvalidArtifact))
		Expect(err).To(MatchError(ContainSubstring(message)))
	},
		Entry("a multi-arch VM without arch", artifacttest.VMMultiArch, "", "several architectures"),
		Entry("a multi-arch VM with a missing arch", artifacttest.VMMultiArch, "s390x", "no manifest for architecture"),
		Entry("a plain manifest with another arch", artifacttest.VMPlainManifest, artifacttest.ArchARM64,
			`artifact is for architecture "amd64", "arm64" was requested`),
		Entry(artifacttest.ArchMismatch, artifacttest.ArchMismatch, "",
			`index platform architecture "arm64" does not match config architecture "amd64"`),
		Entry(artifacttest.BadArtifactType, artifacttest.BadArtifactType, "", "unsupported artifactType"),
		Entry(artifacttest.MissingSize, artifacttest.MissingSize, "", "no "+artifact.AnnotationDiskSize),
		Entry(artifacttest.DuplicateDisk, artifacttest.DuplicateDisk, "", "named by more than one layer"),
		Entry(artifacttest.OrphanLayer, artifacttest.OrphanLayer, "", "does not name a PVC volume"),
		Entry(artifacttest.UnlayeredPVC, artifacttest.UnlayeredPVC, "", "is not named by any disk layer"),
		Entry(artifacttest.UnknownLayerMediaType, artifacttest.UnknownLayerMediaType, "", "unsupported media type"),
	)

	It("should fail without marking the artifact invalid when it cannot be accessed", func() {
		ref, err := layout.NewReference(GinkgoT().TempDir(), "")
		Expect(err).ToNot(HaveOccurred())
		_, err = fetcher.Fetch(context.Background(), ref, &types.SystemContext{}, "")
		Expect(err).To(HaveOccurred())
		Expect(err).ToNot(MatchError(artifact.ErrInvalidArtifact))
	})
})
