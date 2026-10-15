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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"go.podman.io/image/v5/docker/reference"
	"go.podman.io/image/v5/oci/layout"
	"go.podman.io/image/v5/types"

	"kubevirt.io/virt-import/internal/artifact"
	"kubevirt.io/virt-import/internal/artifact/artifacttest"
	"kubevirt.io/virt-import/internal/fetcher"
)

// pinnedReference is an OCI layout reference that claims to be the docker reference named.
type pinnedReference struct {
	types.ImageReference
	named reference.Named
}

func (r pinnedReference) DockerReference() reference.Named {
	return r.named
}

// pinnedSource is an image source that claims to be read from ref.
type pinnedSource struct {
	types.ImageSource
	ref types.ImageReference
}

func (s pinnedSource) Reference() types.ImageReference {
	return s.ref
}

// failingSource is an image source that fails to fetch any manifest.
type failingSource struct {
	types.ImageSource
}

func (failingSource) GetManifest(context.Context, *digest.Digest) (manifest []byte, mimeType string, err error) {
	return nil, "", errors.New("connection refused")
}

var _ = Describe("ParseURL", func() {
	const (
		sha256Digest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
		sha512Digest = "sha512:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef" +
			"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	)

	DescribeTable("should parse", func(url, expected string) {
		ref, err := fetcher.ParseURL(url)
		Expect(err).ToNot(HaveOccurred())
		Expect(ref.DockerReference().String()).To(Equal(expected))
	},
		Entry("a tag", "docker://quay.io/org/vm:v1", "quay.io/org/vm:v1"),
		Entry("a name only", "docker://quay.io/org/vm", "quay.io/org/vm:latest"),
		Entry("a docker hub name", "docker://org/vm:v1", "docker.io/org/vm:v1"),
		Entry("a sha256 digest", "docker://quay.io/org/vm@"+sha256Digest, "quay.io/org/vm@"+sha256Digest),
		Entry("a sha512 digest", "docker://quay.io/org/vm@"+sha512Digest, "quay.io/org/vm@"+sha512Digest),
		Entry("a tag and a digest", "docker://quay.io/org/vm:v1@"+sha256Digest, "quay.io/org/vm@"+sha256Digest),
	)

	DescribeTable("should reject", func(url, message string) {
		_, err := fetcher.ParseURL(url)
		Expect(err).To(MatchError(ContainSubstring(message)))
	},
		Entry("a url without scheme", "quay.io/org/vm:v1", "has to start with docker://"),
		Entry("an invalid name", "docker://quay.io/Org/vm:v1", "invalid url"),
		Entry("an invalid digest", "docker://quay.io/org/vm@sha256:abc", "invalid url"),
	)
})

var _ = Describe("TerminationMessage", func() {
	const maxLength = 4096

	It("should keep a short message", func() {
		Expect(fetcher.TerminationMessage(errors.New("failed"))).To(Equal("failed"))
	})

	DescribeTable("should cut a long message at its end", func(char string) {
		msg := "failed to access docker://registry/vm: " + strings.Repeat(char, maxLength)
		cut := fetcher.TerminationMessage(errors.New(msg))
		Expect(len(cut)).To(BeNumerically("<=", maxLength))
		Expect(cut).To(HavePrefix("failed to access docker://registry/vm: "))
		Expect(cut).To(HaveSuffix("..."))
		Expect(utf8.ValidString(cut)).To(BeTrue(), "the cut does not split a character")
	},
		Entry("of single byte characters", "x"),
		Entry("of multi byte characters", "\u00e4"),
	)
})

var _ = Describe("Fetch", func() {
	writeLayout := func(a *artifacttest.Artifact) string {
		GinkgoHelper()
		dir := GinkgoT().TempDir()
		Expect(a.WriteLayout(dir)).To(Succeed())
		return dir
	}

	openLayout := func(dir string) types.ImageSource {
		GinkgoHelper()
		ref, err := layout.NewReference(dir, "")
		Expect(err).ToNot(HaveOccurred())
		src, err := ref.NewImageSource(context.Background(), &types.SystemContext{})
		Expect(err).ToNot(HaveOccurred())
		DeferCleanup(src.Close)
		return src
	}

	fetchArtifact := func(a *artifacttest.Artifact, arch string) (*artifact.FetchResult, error) {
		GinkgoHelper()
		return fetcher.Fetch(context.Background(), openLayout(writeLayout(a)), arch)
	}

	// readJSON decodes the blob d of the layout in dir into v.
	readJSON := func(dir string, d digest.Digest, v any) {
		GinkgoHelper()
		content, err := os.ReadFile(filepath.Join(dir, ocispec.ImageBlobsDir, d.Algorithm().String(), d.Encoded()))
		Expect(err).ToNot(HaveOccurred())
		Expect(json.Unmarshal(content, v)).To(Succeed())
	}

	// topDigest returns the digest of the manifest or index the layout in dir is tagged with.
	topDigest := func(dir string) digest.Digest {
		GinkgoHelper()
		indexJSON, err := os.ReadFile(filepath.Join(dir, ocispec.ImageIndexFile))
		Expect(err).ToNot(HaveOccurred())
		index := &ocispec.Index{}
		Expect(json.Unmarshal(indexJSON, index)).To(Succeed())
		Expect(index.Manifests).To(HaveLen(1))
		return index.Manifests[0].Digest
	}

	fetch := func(name, arch string) (*artifact.FetchResult, error) {
		GinkgoHelper()
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
		Entry("a VMTemplate with several kinds of volumes", artifacttest.VMTemplateMultiDisk, "", true,
			artifact.ArtifactTypeVMTemplate, artifacttest.ArchAMD64, artifacttest.RootDisk, artifacttest.DataDisk),
		Entry("the index architecture of a VM without architecture", artifacttest.VMUnsetArch, "", true,
			artifact.ArtifactTypeVM, artifacttest.ArchARM64, artifacttest.RootDisk),
	)

	// fetchPinned fetches a, referenced by the digest pin returns for the digest of its top
	// level manifest.
	fetchPinned := func(a *artifacttest.Artifact, pin func(top digest.Digest) digest.Digest) (*artifact.FetchResult, error) {
		GinkgoHelper()
		dir := writeLayout(a)
		named, err := reference.ParseNormalizedNamed("quay.io/org/vm")
		Expect(err).ToNot(HaveOccurred())
		canonical, err := reference.WithDigest(named, pin(topDigest(dir)))
		Expect(err).ToNot(HaveOccurred())
		src := openLayout(dir)
		pinned := pinnedSource{ImageSource: src, ref: pinnedReference{ImageReference: src.Reference(), named: canonical}}
		return fetcher.Fetch(context.Background(), pinned, "")
	}

	sha512Artifact := func() *artifacttest.Artifact {
		a := artifacttest.Fixtures()[artifacttest.VMSingle]
		a.Algorithm = digest.SHA512
		return a
	}

	It("should fetch an artifact with sha512 digests", func() {
		result, err := fetchArtifact(sha512Artifact(), "")
		Expect(err).ToNot(HaveOccurred())
		// A tag is resolved to the sha256 digest of the index.
		Expect(result.Digest.Algorithm()).To(Equal(digest.SHA256))
		Expect(result.ManifestDigest.Algorithm()).To(Equal(digest.SHA512))
		Expect(result.Layers).To(HaveLen(1))
		Expect(result.Layers[0].Digest.Algorithm()).To(Equal(digest.SHA512))
	})

	It("should keep the sha512 digest of a reference", func() {
		var top digest.Digest
		result, err := fetchPinned(sha512Artifact(), func(d digest.Digest) digest.Digest {
			top = d
			return d
		})
		Expect(err).ToNot(HaveOccurred())
		Expect(top.Algorithm()).To(Equal(digest.SHA512))
		Expect(result.Digest).To(Equal(top))
	})

	It("should reject a manifest that does not match the digest of the reference", func() {
		_, err := fetchPinned(sha512Artifact(), func(digest.Digest) digest.Digest {
			return digest.SHA512.FromString("other")
		})
		Expect(err).To(MatchError(artifact.ErrInvalidArtifact))
		Expect(err).To(MatchError(ContainSubstring("manifest does not match digest")))
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

	// fetchReplacedConfig fetches a plain manifest artifact with its config blob replaced.
	fetchReplacedConfig := func(config []byte) error {
		GinkgoHelper()
		dir := writeLayout(artifacttest.Fixtures()[artifacttest.VMPlainManifest])
		m := &ocispec.Manifest{}
		readJSON(dir, topDigest(dir), m)
		d := m.Config.Digest
		Expect(os.WriteFile(filepath.Join(dir, ocispec.ImageBlobsDir, d.Algorithm().String(), d.Encoded()),
			config, 0o600)).To(Succeed())
		_, err := fetcher.Fetch(context.Background(), openLayout(dir), "")
		return err
	}

	It("should reject a config larger than its descriptor size", func() {
		err := fetchReplacedConfig(bytes.Repeat([]byte("x"), 512*1024+1))
		Expect(err).To(MatchError(artifact.ErrInvalidArtifact))
		Expect(err).To(MatchError(ContainSubstring("larger than its descriptor size")))
	})

	It("should reject a config that does not match its digest", func() {
		err := fetchReplacedConfig([]byte(`{"kind":"VirtualMachine"}`))
		Expect(err).To(MatchError(artifact.ErrInvalidArtifact))
		Expect(err).To(MatchError(ContainSubstring("config does not match digest")))
	})

	// fetchChangedTop fetches the artifact name with its top level index or manifest changed by
	// change. The blob keeps its path, so the layout still finds it.
	fetchChangedTop := func(name string, change func(top map[string]any)) error {
		GinkgoHelper()
		dir := writeLayout(artifacttest.Fixtures()[name])
		d := topDigest(dir)
		top := map[string]any{}
		readJSON(dir, d, &top)
		change(top)
		raw, err := json.Marshal(top)
		Expect(err).ToNot(HaveOccurred())
		Expect(os.WriteFile(filepath.Join(dir, ocispec.ImageBlobsDir, d.Algorithm().String(), d.Encoded()),
			raw, 0o600)).To(Succeed())
		_, err = fetcher.Fetch(context.Background(), openLayout(dir), "")
		return err
	}

	setEntryDigest := func(d string) func(map[string]any) {
		return func(index map[string]any) {
			index["manifests"].([]any)[0].(map[string]any)["digest"] = d
		}
	}

	setConfigDigest := func(d string) func(map[string]any) {
		return func(manifest map[string]any) {
			manifest["config"].(map[string]any)["digest"] = d
		}
	}

	const (
		malformedDigest   = "sha256:abc"
		unsupportedDigest = "md5:d41d8cd98f00b204e9800998ecf8427e"
	)

	DescribeTable("should reject an invalid digest", func(name string, change func(map[string]any), message string) {
		err := fetchChangedTop(name, change)
		Expect(err).To(MatchError(artifact.ErrInvalidArtifact))
		Expect(err).To(MatchError(ContainSubstring(message)))
	},
		Entry("of an index entry", artifacttest.VMSingle, setEntryDigest(malformedDigest),
			`manifest has invalid digest "`+malformedDigest+`"`),
		Entry("of an index entry with an unsupported algorithm", artifacttest.VMSingle, setEntryDigest(unsupportedDigest),
			`manifest has invalid digest "`+unsupportedDigest+`"`),
		Entry("of a config", artifacttest.VMPlainManifest, setConfigDigest(malformedDigest),
			`config has invalid digest "`+malformedDigest+`"`),
		Entry("of a config with an unsupported algorithm", artifacttest.VMPlainManifest, setConfigDigest(unsupportedDigest),
			`config has invalid digest "`+unsupportedDigest+`"`),
	)

	It("should fail without marking the artifact invalid when the manifest cannot be fetched", func() {
		src := openLayout(writeLayout(artifacttest.Fixtures()[artifacttest.VMSingle]))
		_, err := fetcher.Fetch(context.Background(), failingSource{ImageSource: src}, "")
		Expect(err).To(MatchError(ContainSubstring("connection refused")))
		Expect(err).ToNot(MatchError(artifact.ErrInvalidArtifact))
	})
})
