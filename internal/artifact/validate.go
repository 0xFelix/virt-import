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

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

var configMediaTypes = map[string]string{
	ArtifactTypeVM:         MediaTypeVMConfig,
	ArtifactTypeVMTemplate: MediaTypeVMTemplateConfig,
}

// ValidateManifest checks a manifest against the VEP #256 artifact format. indexArtifactType
// is the artifactType of the index the manifest was selected from, empty for a plain manifest.
func ValidateManifest(manifest *ocispec.Manifest, indexArtifactType string) error {
	if manifest.MediaType != ocispec.MediaTypeImageManifest {
		return fmt.Errorf("%w: unsupported manifest media type %q", ErrInvalidArtifact, manifest.MediaType)
	}

	configMediaType, ok := configMediaTypes[manifest.ArtifactType]
	if !ok {
		return fmt.Errorf("%w: unsupported artifactType %q", ErrInvalidArtifact, manifest.ArtifactType)
	}
	if indexArtifactType != "" && indexArtifactType != manifest.ArtifactType {
		return fmt.Errorf("%w: index artifactType %q does not match manifest artifactType %q",
			ErrInvalidArtifact, indexArtifactType, manifest.ArtifactType)
	}
	if manifest.Config.MediaType != configMediaType {
		return fmt.Errorf("%w: config media type %q does not match artifactType %q",
			ErrInvalidArtifact, manifest.Config.MediaType, manifest.ArtifactType)
	}

	names := map[string]struct{}{}
	for _, layer := range manifest.Layers {
		if layer.MediaType != MediaTypeDiskRawZstd {
			return fmt.Errorf("%w: layer %s has unsupported media type %q", ErrInvalidArtifact, layer.Digest, layer.MediaType)
		}
		name := layer.Annotations[AnnotationDiskName]
		if name == "" {
			return fmt.Errorf("%w: layer %s has no %s annotation", ErrInvalidArtifact, layer.Digest, AnnotationDiskName)
		}
		if _, dup := names[name]; dup {
			return fmt.Errorf("%w: disk %q is named by more than one layer", ErrInvalidArtifact, name)
		}
		names[name] = struct{}{}
		size, ok := layer.Annotations[AnnotationDiskSize]
		if !ok {
			return fmt.Errorf("%w: layer %s has no %s annotation", ErrInvalidArtifact, layer.Digest, AnnotationDiskSize)
		}
		if quantity, err := resource.ParseQuantity(size); err != nil || quantity.Sign() <= 0 {
			return fmt.Errorf("%w: layer %s has invalid %s %q", ErrInvalidArtifact, layer.Digest, AnnotationDiskSize, size)
		}
	}

	return nil
}
