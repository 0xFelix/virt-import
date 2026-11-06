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

package artifact

import (
	"slices"
	"strings"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

const platformUnknown = "unknown"

// SelectManifest returns the index entry for arch. Like CDI, it only considers linux entries with
// a known architecture. Without arch, the index must have exactly one of them.
func SelectManifest(index *ocispec.Index, arch string) (*ocispec.Descriptor, error) {
	var known, candidates []ocispec.Descriptor
	for _, desc := range index.Manifests {
		if !isKnownPlatform(desc.Platform) {
			continue
		}
		known = append(known, desc)
		if arch == "" || desc.Platform.Architecture == arch {
			candidates = append(candidates, desc)
		}
	}

	switch {
	case len(candidates) == 1:
		if candidates[0].MediaType != ocispec.MediaTypeImageManifest {
			return nil, InvalidArtifactf("index entry %s has unsupported media type %q",
				candidates[0].Digest, candidates[0].MediaType)
		}
		return &candidates[0], nil
	case arch == "" && len(candidates) == 0:
		return nil, InvalidArtifactf("index has no linux manifest for a known architecture")
	case arch == "":
		return nil, InvalidArtifactf(
			"index has manifests for several architectures (%s), platform.architecture has to select one",
			strings.Join(architectures(candidates), ", "),
		)
	case len(candidates) == 0:
		return nil, InvalidArtifactf("index has no manifest for architecture %q, available: %s",
			arch, strings.Join(architectures(known), ", "))
	default:
		return nil, InvalidArtifactf("index has %d manifests for architecture %q", len(candidates), arch)
	}
}

// ResolveArchitecture returns the architecture of the selected manifest. For an index that is
// indexArch, and the config must not set a different one. For a plain manifest (indexArch empty)
// it is the config's architecture, or DefaultArchitecture.
func ResolveArchitecture(artifactType string, config *unstructured.Unstructured, indexArch string) (string, error) {
	// The exporter fills the index platform from spec.template.spec.architecture of the
	// VirtualMachine, for a VirtualMachineTemplate with parameter defaults substituted.
	archPath := vmPath(artifactType, fieldSpec, fieldTemplate, fieldSpec, "architecture")
	configArch, _, _ := unstructured.NestedString(config.Object, archPath...)
	if artifactType == ArtifactTypeVMTemplate {
		configArch = resolveParameters(configArch, config.Object)
	}
	// A "${" left after the substitution is a parameter without default value. The exporter
	// treats it like an unset architecture, see ResolveParameterValue in kubevirt/kubevirt
	// pkg/storage/export/export.
	set := configArch != "" && !strings.Contains(configArch, "${")

	if indexArch == "" {
		if !set {
			return DefaultArchitecture, nil
		}
		return configArch, nil
	}
	if set && configArch != indexArch {
		return "", InvalidArtifactf("index platform architecture %q does not match config architecture %q",
			indexArch, configArch)
	}
	return indexArch, nil
}

// resolveParameters substitutes ${NAME} with the default value of each template parameter
// that has one, like ResolveParameterValue in kubevirt/kubevirt pkg/storage/export/export.
func resolveParameters(s string, tpl map[string]any) string {
	params, _, _ := unstructured.NestedSlice(tpl, fieldSpec, "parameters")
	for _, p := range params {
		param, ok := p.(map[string]any)
		if !ok {
			continue
		}
		name, _, _ := unstructured.NestedString(param, fieldName)
		value, _, _ := unstructured.NestedString(param, "value")
		if name == "" || value == "" {
			continue
		}
		s = strings.ReplaceAll(s, "${"+name+"}", value)
	}
	return s
}

func isKnownPlatform(p *ocispec.Platform) bool {
	return p != nil &&
		p.Architecture != "" && p.Architecture != platformUnknown &&
		p.OS == SupportedOS
}

func architectures(descs []ocispec.Descriptor) []string {
	archs := make([]string, 0, len(descs))
	for _, desc := range descs {
		archs = append(archs, desc.Platform.Architecture)
	}
	slices.Sort(archs)
	return archs
}
