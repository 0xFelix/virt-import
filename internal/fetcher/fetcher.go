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

// Package fetcher reads the metadata of an OCI artifact from a registry.
package fetcher

import (
	"context"
	// Registers sha512 for digest verification.
	_ "crypto/sha512"
	"encoding/json"
	"fmt"
	"io"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"go.podman.io/image/v5/manifest"
	"go.podman.io/image/v5/pkg/blobinfocache/none"
	"go.podman.io/image/v5/transports"
	"go.podman.io/image/v5/types"

	"kubevirt.io/virt-import/internal/artifact"
)

// maxConfigSize bounds the config blob. The rewritten object has to fit into a ConfigMap.
const maxConfigSize = 512 * 1024

func Fetch(ctx context.Context, ref types.ImageReference, sys *types.SystemContext, arch string) (*artifact.FetchResult, error) {
	src, err := ref.NewImageSource(ctx, sys)
	if err != nil {
		return nil, fmt.Errorf("failed to access %s: %w", transports.ImageName(ref), err)
	}
	defer src.Close()

	result, m, err := resolveManifest(ctx, src, arch)
	if err != nil {
		return nil, err
	}
	if result.Config, err = fetchConfig(ctx, src, m.Config); err != nil {
		return nil, err
	}
	config, err := artifact.ValidateConfig(result.ArtifactType, result.Config, result.DiskNames())
	if err != nil {
		return nil, err
	}

	if result.Architecture, err = artifact.ResolveArchitecture(result.ArtifactType, config, result.Architecture); err != nil {
		return nil, err
	}
	if !result.IsIndex && arch != "" && arch != result.Architecture {
		return nil, fmt.Errorf("%w: artifact is for architecture %q, %q was requested",
			artifact.ErrInvalidArtifact, result.Architecture, arch)
	}

	return result, nil
}

// resolveManifest fetches the manifest ref points to, selecting it from an image index by arch,
// and validates it. The returned result holds everything but the config.
func resolveManifest(ctx context.Context, src types.ImageSource, arch string) (*artifact.FetchResult, *ocispec.Manifest, error) {
	raw, mimeType, err := src.GetManifest(ctx, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to fetch the manifest: %w", err)
	}
	if mimeType == "" {
		mimeType = manifest.GuessMIMEType(raw)
	}

	result := &artifact.FetchResult{Digest: digest.FromBytes(raw)}
	indexArtifactType := ""
	switch mimeType {
	case ocispec.MediaTypeImageIndex:
		index := &ocispec.Index{}
		if err = json.Unmarshal(raw, index); err != nil {
			return nil, nil, fmt.Errorf("%w: image index cannot be decoded: %w", artifact.ErrInvalidArtifact, err)
		}
		var desc ocispec.Descriptor
		if desc, err = artifact.SelectManifest(index, arch); err != nil {
			return nil, nil, err
		}
		if raw, err = fetchManifest(ctx, src, desc.Digest); err != nil {
			return nil, nil, err
		}
		result.IsIndex = true
		result.ManifestDigest = desc.Digest
		result.Architecture = desc.Platform.Architecture
		indexArtifactType = index.ArtifactType
	case ocispec.MediaTypeImageManifest:
		result.ManifestDigest = result.Digest
	default:
		return nil, nil, fmt.Errorf("%w: unsupported manifest media type %q", artifact.ErrInvalidArtifact, mimeType)
	}

	m := &ocispec.Manifest{}
	if err = json.Unmarshal(raw, m); err != nil {
		return nil, nil, fmt.Errorf("%w: manifest cannot be decoded: %w", artifact.ErrInvalidArtifact, err)
	}
	if err := artifact.ValidateManifest(m, indexArtifactType); err != nil {
		return nil, nil, err
	}
	result.ArtifactType = m.ArtifactType
	result.Layers = artifact.LayersFromManifest(m)

	return result, m, nil
}

func fetchManifest(ctx context.Context, src types.ImageSource, d digest.Digest) ([]byte, error) {
	if err := validateDigest("manifest", d); err != nil {
		return nil, err
	}
	raw, _, err := src.GetManifest(ctx, &d)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch manifest %s: %w", d, err)
	}
	if d.Algorithm().FromBytes(raw) != d {
		return nil, fmt.Errorf("%w: manifest does not match digest %s", artifact.ErrInvalidArtifact, d)
	}
	return raw, nil
}

func fetchConfig(ctx context.Context, src types.ImageSource, desc ocispec.Descriptor) ([]byte, error) {
	if desc.Size > maxConfigSize {
		return nil, fmt.Errorf("%w: config is %d bytes, at most %d are supported", artifact.ErrInvalidArtifact, desc.Size, maxConfigSize)
	}
	if err := validateDigest("config", desc.Digest); err != nil {
		return nil, err
	}

	rc, _, err := src.GetBlob(ctx, types.BlobInfo{Digest: desc.Digest, Size: desc.Size}, none.NoCache)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch config %s: %w", desc.Digest, err)
	}
	defer rc.Close()

	config, err := io.ReadAll(io.LimitReader(rc, maxConfigSize+1))
	if err != nil {
		return nil, fmt.Errorf("failed to read config %s: %w", desc.Digest, err)
	}
	if len(config) > maxConfigSize || desc.Digest.Algorithm().FromBytes(config) != desc.Digest {
		return nil, fmt.Errorf("%w: config does not match digest %s", artifact.ErrInvalidArtifact, desc.Digest)
	}
	return config, nil
}

func validateDigest(what string, d digest.Digest) error {
	if err := d.Validate(); err != nil {
		return fmt.Errorf("%w: %s has invalid digest %q: %w", artifact.ErrInvalidArtifact, what, d, err)
	}
	return nil
}
