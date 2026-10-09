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

var _ = Describe("Platform", func() {
	const (
		amd64   = "amd64"
		arm64   = "arm64"
		s390x   = "s390x"
		linux   = "linux"
		windows = "windows"
		unknown = "unknown"
	)

	manifestFor := func(arch, os string) ocispec.Descriptor {
		return ocispec.Descriptor{
			MediaType: ocispec.MediaTypeImageManifest,
			Digest:    digest.FromString(arch + "/" + os),
			Platform:  &ocispec.Platform{Architecture: arch, OS: os},
		}
	}

	indexOf := func(descs ...ocispec.Descriptor) *ocispec.Index {
		return &ocispec.Index{Manifests: descs}
	}

	Context("SelectManifest", func() {
		DescribeTable("should select", func(index *ocispec.Index, arch, expectedArch string) {
			desc, err := artifact.SelectManifest(index, arch)
			Expect(err).ToNot(HaveOccurred())
			Expect(desc.Platform.Architecture).To(Equal(expectedArch))
			Expect(desc.Platform.OS).To(Equal(linux))
		},
			Entry("the only manifest without arch", indexOf(manifestFor(amd64, linux)), "", amd64),
			Entry("the requested arch", indexOf(manifestFor(amd64, linux), manifestFor(arm64, linux)), arm64, arm64),
			Entry("the only known platform, skipping unknown ones",
				indexOf(manifestFor(amd64, linux), manifestFor(unknown, unknown), manifestFor("", linux), manifestFor(arm64, "")),
				"", amd64),
			Entry("the linux manifest, skipping other OSes",
				indexOf(manifestFor(amd64, windows), manifestFor(amd64, linux)), amd64, amd64),
		)

		DescribeTable("should reject", func(index *ocispec.Index, arch, message string) {
			_, err := artifact.SelectManifest(index, arch)
			Expect(err).To(MatchError(artifact.ErrInvalidArtifact))
			Expect(err).To(MatchError(ContainSubstring(message)))
		},
			Entry("an index without known platforms", indexOf(manifestFor(unknown, unknown)), "",
				"no linux manifest for a known architecture"),
			Entry("an index without linux manifests", indexOf(manifestFor(amd64, windows)), "",
				"no linux manifest for a known architecture"),
			Entry("several architectures without arch", indexOf(manifestFor(arm64, linux), manifestFor(amd64, linux)), "",
				"several architectures (amd64, arm64)"),
			Entry("a missing arch", indexOf(manifestFor(amd64, linux), manifestFor(arm64, linux)), s390x,
				`no manifest for architecture "s390x", available: amd64, arm64`),
			Entry("an ambiguous arch", indexOf(manifestFor(amd64, linux), ocispec.Descriptor{
				MediaType: ocispec.MediaTypeImageManifest,
				Digest:    digest.FromString("v2"),
				Platform:  &ocispec.Platform{Architecture: amd64, OS: linux, Variant: "v2"},
			}), amd64, `2 manifests for architecture "amd64"`),
			Entry("a nested index", indexOf(ocispec.Descriptor{
				MediaType: ocispec.MediaTypeImageIndex,
				Digest:    digest.FromString("nested"),
				Platform:  &ocispec.Platform{Architecture: amd64, OS: linux},
			}), "", "unsupported media type"),
		)
	})

	Context("ResolveArchitecture", func() {
		const (
			vmARM64 = `{"apiVersion":"kubevirt.io/v1","kind":"VirtualMachine",
				"spec":{"template":{"spec":{"architecture":"arm64"}}}}`
			vmUnset       = `{"apiVersion":"kubevirt.io/v1","kind":"VirtualMachine","spec":{"template":{"spec":{}}}}`
			templateParam = `{"apiVersion":"template.kubevirt.io/v1beta1","kind":"VirtualMachineTemplate",
				"spec":{"parameters":[{"name":"ARCH"}],
				"virtualMachine":{"spec":{"template":{"spec":{"architecture":"${ARCH}"}}}}}}`
		)

		resolve := func(artifactType, config, indexArch string) (string, error) {
			obj, err := artifact.ValidateConfig(artifactType, []byte(config), nil)
			ExpectWithOffset(1, err).ToNot(HaveOccurred())
			return artifact.ResolveArchitecture(artifactType, obj, indexArch)
		}

		DescribeTable("should derive from the config of a plain manifest", func(artifactType, config, expected string) {
			Expect(resolve(artifactType, config, "")).To(Equal(expected))
		},
			Entry("a VM architecture", artifact.ArtifactTypeVM, vmARM64, arm64),
			Entry("the default for a VM without architecture", artifact.ArtifactTypeVM, vmUnset, amd64),
			Entry("a literal template architecture", artifact.ArtifactTypeVMTemplate,
				`{"apiVersion":"template.kubevirt.io/v1beta1","kind":"VirtualMachineTemplate",
				"spec":{"virtualMachine":{"spec":{"template":{"spec":{"architecture":"s390x"}}}}}}`, s390x),
			Entry("a template architecture from a parameter default", artifact.ArtifactTypeVMTemplate,
				`{"apiVersion":"template.kubevirt.io/v1beta1","kind":"VirtualMachineTemplate",
				"spec":{"parameters":[{"name":"ARCH","value":"arm64"}],
				"virtualMachine":{"spec":{"template":{"spec":{"architecture":"${ARCH}"}}}}}}`, arm64),
			Entry("the default for an unresolvable template parameter", artifact.ArtifactTypeVMTemplate, templateParam, amd64),
		)

		DescribeTable("should keep the index architecture", func(artifactType, config string) {
			Expect(resolve(artifactType, config, arm64)).To(Equal(arm64))
		},
			Entry("when the config matches", artifact.ArtifactTypeVM, vmARM64),
			Entry("when the VM sets no architecture", artifact.ArtifactTypeVM, vmUnset),
			Entry("when a template parameter is unresolvable", artifact.ArtifactTypeVMTemplate, templateParam),
		)

		It("should reject an index architecture the config contradicts", func() {
			_, err := resolve(artifact.ArtifactTypeVM, vmARM64, amd64)
			Expect(err).To(MatchError(artifact.ErrInvalidArtifact))
			Expect(err).To(MatchError(ContainSubstring(`index platform architecture "amd64" does not match config architecture "arm64"`)))
		})
	})
})
