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
	"kubevirt.io/virt-import/internal/fetcher"
	"kubevirt.io/virt-import/internal/testing/artifacts"
)

var _ = Describe("Fetch", func() {
	fetch := func(name, arch string) (*artifact.FetchResult, error) {
		dir := GinkgoT().TempDir()
		ExpectWithOffset(1, artifacts.Fixtures()[name].WriteLayout(dir)).To(Succeed())
		ref, err := layout.NewReference(dir, "")
		ExpectWithOffset(1, err).ToNot(HaveOccurred())
		return fetcher.Fetch(context.Background(), ref, &types.SystemContext{}, arch)
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
			Expect(layer.Digest.Algorithm()).To(Equal(digest.SHA256))
		}
	},
		Entry("a single disk VM", artifacts.VMSingle, "", true, artifact.ArtifactTypeVM, artifacts.ArchAMD64,
			artifacts.RootDisk),
		Entry("a multi disk VM", artifacts.VMMultiDisk, "", true, artifact.ArtifactTypeVM, artifacts.ArchAMD64,
			artifacts.RootDisk, artifacts.DataDisk),
		Entry("amd64 of a multi-arch VM", artifacts.VMMultiArch, artifacts.ArchAMD64, true, artifact.ArtifactTypeVM,
			artifacts.ArchAMD64, artifacts.RootDisk, artifacts.DataDisk),
		Entry("arm64 of a multi-arch VM", artifacts.VMMultiArch, artifacts.ArchARM64, true, artifact.ArtifactTypeVM,
			artifacts.ArchARM64, artifacts.RootDisk),
		Entry("a plain manifest", artifacts.VMPlainManifest, "", false, artifact.ArtifactTypeVM, artifacts.ArchAMD64,
			artifacts.RootDisk),
		Entry("a plain manifest with matching arch", artifacts.VMPlainManifest, artifacts.ArchAMD64, false,
			artifact.ArtifactTypeVM, artifacts.ArchAMD64, artifacts.RootDisk),
		Entry("a VMTemplate", artifacts.VMTemplateDVT, "", true, artifact.ArtifactTypeVMTemplate, artifacts.ArchAMD64,
			artifacts.RootDisk),
	)

	DescribeTable("should reject", func(name, arch, message string) {
		_, err := fetch(name, arch)
		Expect(err).To(MatchError(artifact.ErrInvalidArtifact))
		Expect(err).To(MatchError(ContainSubstring(message)))
	},
		Entry("a multi-arch VM without arch", artifacts.VMMultiArch, "", "several architectures"),
		Entry("a multi-arch VM with a missing arch", artifacts.VMMultiArch, "s390x", "no manifest for architecture"),
		Entry("a plain manifest with another arch", artifacts.VMPlainManifest, artifacts.ArchARM64,
			`artifact is for architecture "amd64", "arm64" was requested`),
		Entry(artifacts.BadArtifactType, artifacts.BadArtifactType, "", "unsupported artifactType"),
		Entry(artifacts.MissingSize, artifacts.MissingSize, "", "no "+artifact.AnnotationDiskSize),
		Entry(artifacts.DuplicateDisk, artifacts.DuplicateDisk, "", "named by more than one layer"),
		Entry(artifacts.OrphanLayer, artifacts.OrphanLayer, "", "does not name a PVC volume"),
		Entry(artifacts.UnlayeredPVC, artifacts.UnlayeredPVC, "", "is not named by any disk layer"),
		Entry(artifacts.UnknownLayerMediaType, artifacts.UnknownLayerMediaType, "", "unsupported media type"),
	)

	It("should fail without marking the artifact invalid when it cannot be accessed", func() {
		ref, err := layout.NewReference(GinkgoT().TempDir(), "")
		Expect(err).ToNot(HaveOccurred())
		_, err = fetcher.Fetch(context.Background(), ref, &types.SystemContext{}, "")
		Expect(err).To(HaveOccurred())
		Expect(err).ToNot(MatchError(artifact.ErrInvalidArtifact))
	})
})
