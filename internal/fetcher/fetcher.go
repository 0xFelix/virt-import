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
	"strings"
	"unicode/utf8"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"go.podman.io/image/v5/docker"
	"go.podman.io/image/v5/docker/reference"
	"go.podman.io/image/v5/manifest"
	"go.podman.io/image/v5/pkg/blobinfocache/none"
	"go.podman.io/image/v5/types"

	"kubevirt.io/virt-import/internal/artifact"
)

// ParseURL parses a docker:// URL like CDI does. A tag next to a digest is dropped, since the
// digest decides what is fetched.
func ParseURL(url string) (types.ImageReference, error) {
	name, ok := strings.CutPrefix(url, artifact.DockerScheme)
	if !ok {
		return nil, fmt.Errorf("url %q has to start with %s", url, artifact.DockerScheme)
	}
	named, err := reference.ParseNormalizedNamed(name)
	if err != nil {
		return nil, fmt.Errorf("invalid url %q: %w", url, err)
	}
	if canonical, ok := named.(reference.Canonical); ok {
		if named, err = reference.WithDigest(reference.TrimNamed(named), canonical.Digest()); err != nil {
			return nil, fmt.Errorf("invalid url %q: %w", url, err)
		}
	}
	ref, err := docker.NewReference(reference.TagNameOnly(named))
	if err != nil {
		return nil, fmt.Errorf("invalid url %q: %w", url, err)
	}
	return ref, nil
}

// maxConfigSize bounds the config blob. The rewritten object has to fit into a ConfigMap.
const maxConfigSize = 512 * 1024

// Fetch reads and validates the metadata of the artifact behind src. A digest reference keeps its
// digest in the result.
func Fetch(ctx context.Context, src types.ImageSource, arch string) (*artifact.FetchResult, error) {
	var pinned digest.Digest
	if canonical, ok := src.Reference().DockerReference().(reference.Canonical); ok {
		pinned = canonical.Digest()
	}
	result, m, err := resolveManifest(ctx, src, pinned, arch)
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
		return nil, artifact.InvalidArtifactf("artifact is for architecture %q, %q was requested",
			result.Architecture, arch)
	}

	return result, nil
}

// resolveManifest fetches and validates the manifest, selecting it by arch if src points to an
// index. The result is complete except for the config. pinned is the digest of the reference, or
// empty for a tag, which then resolves to its sha256 digest.
func resolveManifest(
	ctx context.Context, src types.ImageSource, pinned digest.Digest, arch string,
) (*artifact.FetchResult, *ocispec.Manifest, error) {
	raw, mimeType, err := src.GetManifest(ctx, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to fetch the manifest: %w", err)
	}
	if mimeType == "" {
		mimeType = manifest.GuessMIMEType(raw)
	}

	result := &artifact.FetchResult{Digest: pinned}
	if pinned == "" {
		result.Digest = digest.FromBytes(raw)
	} else if pinned.Algorithm().FromBytes(raw) != pinned {
		return nil, nil, artifact.InvalidArtifactf("manifest does not match digest %s", pinned)
	}
	indexArtifactType := ""
	switch mimeType {
	case ocispec.MediaTypeImageIndex:
		index := &ocispec.Index{}
		if err = json.Unmarshal(raw, index); err != nil {
			return nil, nil, artifact.InvalidArtifactf("image index cannot be decoded: %w", err)
		}
		var desc *ocispec.Descriptor
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
		return nil, nil, artifact.InvalidArtifactf("unsupported manifest media type %q", mimeType)
	}

	m := &ocispec.Manifest{}
	if err = json.Unmarshal(raw, m); err != nil {
		return nil, nil, artifact.InvalidArtifactf("manifest cannot be decoded: %w", err)
	}
	if err := artifact.ValidateManifest(m, indexArtifactType); err != nil {
		return nil, nil, err
	}
	result.ArtifactType = m.ArtifactType
	result.Layers = artifact.LayersFromManifest(m)

	return result, m, nil
}

func fetchManifest(ctx context.Context, src types.ImageSource, d digest.Digest) ([]byte, error) {
	if err := d.Validate(); err != nil {
		return nil, artifact.InvalidArtifactf("manifest has invalid digest %q: %w", d, err)
	}
	raw, _, err := src.GetManifest(ctx, &d)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch manifest %s: %w", d, err)
	}
	if d.Algorithm().FromBytes(raw) != d {
		return nil, artifact.InvalidArtifactf("manifest does not match digest %s", d)
	}
	return raw, nil
}

func fetchConfig(ctx context.Context, src types.ImageSource, desc ocispec.Descriptor) ([]byte, error) {
	if desc.Size > maxConfigSize {
		return nil, artifact.InvalidArtifactf("config is %d bytes, at most %d are supported", desc.Size, maxConfigSize)
	}
	if err := desc.Digest.Validate(); err != nil {
		return nil, artifact.InvalidArtifactf("config has invalid digest %q: %w", desc.Digest, err)
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
	if len(config) > maxConfigSize {
		return nil, artifact.InvalidArtifactf("config %s is larger than its descriptor size, at most %d bytes are supported",
			desc.Digest, maxConfigSize)
	}
	if desc.Digest.Algorithm().FromBytes(config) != desc.Digest {
		return nil, artifact.InvalidArtifactf("config does not match digest %s", desc.Digest)
	}
	return config, nil
}

// maxTerminationMessageLength is how much of a termination message the kubelet keeps. It keeps
// the end, so TerminationMessage cuts longer messages itself to keep their start.
const maxTerminationMessageLength = 4096

// TerminationMessage returns the message of err for the termination log, cut at
// maxTerminationMessageLength.
func TerminationMessage(err error) string {
	const ellipsis = "..."
	msg := err.Error()
	if len(msg) <= maxTerminationMessageLength {
		return msg
	}
	end := maxTerminationMessageLength - len(ellipsis)
	for end > 0 && !utf8.RuneStart(msg[end]) {
		end--
	}
	return msg[:end] + ellipsis
}
