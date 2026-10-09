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
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

const platformUnknown = "unknown"

// SelectManifest returns the manifest of an image index matching arch. Entries with an
// empty or unknown architecture or OS are skipped. With arch set exactly one entry must
// match it; with arch unset the index must hold exactly one usable entry.
func SelectManifest(index *ocispec.Index, arch string) (ocispec.Descriptor, error) {
	var candidates []ocispec.Descriptor
	for _, desc := range index.Manifests {
		if !isKnownPlatform(desc.Platform) {
			continue
		}
		if arch != "" && desc.Platform.Architecture != arch {
			continue
		}
		candidates = append(candidates, desc)
	}

	switch {
	case len(candidates) == 1:
		if candidates[0].MediaType != ocispec.MediaTypeImageManifest {
			return ocispec.Descriptor{}, fmt.Errorf("%w: index entry %s has unsupported media type %q",
				ErrInvalidArtifact, candidates[0].Digest, candidates[0].MediaType)
		}
		return candidates[0], nil
	case arch == "" && len(candidates) == 0:
		return ocispec.Descriptor{}, fmt.Errorf("%w: index has no manifest for a known platform", ErrInvalidArtifact)
	case arch == "":
		return ocispec.Descriptor{}, fmt.Errorf(
			"%w: index has manifests for several architectures (%s), platform.architecture has to select one",
			ErrInvalidArtifact, strings.Join(architectures(candidates), ", "),
		)
	case len(candidates) == 0:
		return ocispec.Descriptor{}, fmt.Errorf("%w: index has no manifest for architecture %q, available: %s",
			ErrInvalidArtifact, arch, strings.Join(architectures(knownPlatforms(index.Manifests)), ", "))
	default:
		return ocispec.Descriptor{}, fmt.Errorf("%w: index has %d manifests for architecture %q",
			ErrInvalidArtifact, len(candidates), arch)
	}
}

// ConfigArchitecture derives the architecture of a config blob the way the exporter does
// when it fills the index platform: the VirtualMachine's spec.template.spec.architecture,
// for a VirtualMachineTemplate with parameter defaults substituted. An empty or unresolvable
// architecture yields DefaultArchitecture.
func ConfigArchitecture(artifactType string, config []byte) (string, error) {
	obj := map[string]any{}
	if err := json.Unmarshal(config, &obj); err != nil {
		return "", fmt.Errorf("%w: config is not a JSON object: %w", ErrInvalidArtifact, err)
	}

	var arch string
	switch artifactType {
	case ArtifactTypeVM:
		arch, _, _ = unstructured.NestedString(obj, fieldSpec, "template", fieldSpec, "architecture")
	case ArtifactTypeVMTemplate:
		arch, _, _ = unstructured.NestedString(obj, fieldSpec, fieldVirtualMachine, fieldSpec, "template", fieldSpec, "architecture")
		arch = resolveParameters(arch, obj)
	default:
		return "", fmt.Errorf("%w: unsupported artifactType %q", ErrInvalidArtifact, artifactType)
	}

	if arch == "" || strings.Contains(arch, "${") {
		return DefaultArchitecture, nil
	}
	return arch, nil
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
		p.OS != "" && p.OS != platformUnknown
}

func knownPlatforms(descs []ocispec.Descriptor) []ocispec.Descriptor {
	var known []ocispec.Descriptor
	for _, desc := range descs {
		if isKnownPlatform(desc.Platform) {
			known = append(known, desc)
		}
	}
	return known
}

func architectures(descs []ocispec.Descriptor) []string {
	archs := make([]string, 0, len(descs))
	for _, desc := range descs {
		archs = append(archs, desc.Platform.Architecture)
	}
	slices.Sort(archs)
	return archs
}
