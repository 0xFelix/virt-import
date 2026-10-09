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
	"fmt"
	"slices"
	"strings"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

const platformUnknown = "unknown"

// SelectManifest returns the manifest of an image index matching arch. Only linux entries with a
// known architecture are considered, since CDI selects from those too. With arch set exactly one
// entry must match it; with arch unset the index must hold exactly one usable entry.
func SelectManifest(index *ocispec.Index, arch string) (ocispec.Descriptor, error) {
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
			return ocispec.Descriptor{}, fmt.Errorf("%w: index entry %s has unsupported media type %q",
				ErrInvalidArtifact, candidates[0].Digest, candidates[0].MediaType)
		}
		return candidates[0], nil
	case arch == "" && len(candidates) == 0:
		return ocispec.Descriptor{}, fmt.Errorf("%w: index has no linux manifest for a known architecture", ErrInvalidArtifact)
	case arch == "":
		return ocispec.Descriptor{}, fmt.Errorf(
			"%w: index has manifests for several architectures (%s), platform.architecture has to select one",
			ErrInvalidArtifact, strings.Join(architectures(candidates), ", "),
		)
	case len(candidates) == 0:
		return ocispec.Descriptor{}, fmt.Errorf("%w: index has no manifest for architecture %q, available: %s",
			ErrInvalidArtifact, arch, strings.Join(architectures(known), ", "))
	default:
		return ocispec.Descriptor{}, fmt.Errorf("%w: index has %d manifests for architecture %q",
			ErrInvalidArtifact, len(candidates), arch)
	}
}

// ResolveArchitecture returns the architecture of a manifest: indexArch, the platform it was
// selected by from an index, or for a plain manifest (indexArch empty) that of its config. A
// config that sets an architecture must match indexArch. config has to come from ValidateConfig.
func ResolveArchitecture(artifactType string, config *unstructured.Unstructured, indexArch string) (string, error) {
	configArch, set := configArchitecture(artifactType, config)
	if indexArch == "" {
		return configArch, nil
	}
	if set && configArch != indexArch {
		return "", fmt.Errorf("%w: index platform architecture %q does not match config architecture %q",
			ErrInvalidArtifact, indexArch, configArch)
	}
	return indexArch, nil
}

// configArchitecture derives the architecture of a config the way the exporter does when it
// fills the index platform: the VirtualMachine's spec.template.spec.architecture, for a
// VirtualMachineTemplate with parameter defaults substituted. An empty or unresolvable
// architecture yields DefaultArchitecture with set false.
func configArchitecture(artifactType string, config *unstructured.Unstructured) (arch string, set bool) {
	arch, _, _ = unstructured.NestedString(config.Object, vmPath(artifactType, fieldSpec, fieldTemplate, fieldSpec, "architecture")...)
	if artifactType == ArtifactTypeVMTemplate {
		arch = resolveParameters(arch, config.Object)
	}

	if arch == "" || strings.Contains(arch, "${") {
		return DefaultArchitecture, false
	}
	return arch, true
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
		p.OS == OS
}

func architectures(descs []ocispec.Descriptor) []string {
	archs := make([]string, 0, len(descs))
	for _, desc := range descs {
		archs = append(archs, desc.Platform.Architecture)
	}
	slices.Sort(archs)
	return archs
}
