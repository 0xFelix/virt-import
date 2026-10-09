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
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// maxResultLineSize bounds a single log line holding a FetchResult. The config blob it embeds
// has to fit into a ConfigMap later anyway.
const maxResultLineSize = 2 * 1024 * 1024

// FetchResult is what the fetcher reports about an artifact.
type FetchResult struct {
	// ArtifactType is the artifactType of the selected manifest.
	ArtifactType string `json:"artifactType"`
	// Architecture is the resolved platform architecture.
	Architecture string `json:"architecture"`
	// IsIndex reports whether the reference resolved to an image index.
	IsIndex bool `json:"isIndex"`
	// Digest pins the reference: the index digest if IsIndex, otherwise the manifest digest.
	Digest digest.Digest `json:"digest"`
	// ManifestDigest is the digest of the selected manifest.
	ManifestDigest digest.Digest `json:"manifestDigest"`
	// Config is the config blob.
	Config json.RawMessage `json:"config,omitempty"`
	// Layers describes the disk layers.
	Layers []Layer `json:"layers"`
}

// Layer describes a disk layer.
type Layer struct {
	Digest   digest.Digest `json:"digest"`
	DiskName string        `json:"diskName"`
	DiskSize string        `json:"diskSize"`
}

type resultLine struct {
	FetchResult *FetchResult `json:"fetchResult"`
}

// LayersFromManifest returns the disk layers of a validated manifest.
func LayersFromManifest(manifest *ocispec.Manifest) []Layer {
	layers := make([]Layer, 0, len(manifest.Layers))
	for _, layer := range manifest.Layers {
		layers = append(layers, Layer{
			Digest:   layer.Digest,
			DiskName: layer.Annotations[AnnotationDiskName],
			DiskSize: layer.Annotations[AnnotationDiskSize],
		})
	}
	return layers
}

// DiskNames returns the disk names of the layers.
func (r *FetchResult) DiskNames() []string {
	names := make([]string, 0, len(r.Layers))
	for _, layer := range r.Layers {
		names = append(names, layer.DiskName)
	}
	return names
}

// WriteResult writes result as a single {"fetchResult":...} line.
func WriteResult(w io.Writer, result *FetchResult) error {
	line, err := json.Marshal(resultLine{FetchResult: result})
	if err != nil {
		return err
	}
	_, err = w.Write(append(line, '\n'))
	return err
}

// ParseResult finds the single {"fetchResult":...} line in the fetcher output. Pod logs merge
// stdout and stderr, so every other line, JSON or not, is ignored.
func ParseResult(r io.Reader) (*FetchResult, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, bufio.MaxScanTokenSize), maxResultLineSize)

	var found *FetchResult
	for scanner.Scan() {
		line := scanner.Bytes()
		if !bytes.HasPrefix(line, []byte("{")) {
			continue
		}
		parsed := resultLine{}
		if err := json.Unmarshal(line, &parsed); err != nil || parsed.FetchResult == nil {
			continue
		}
		if found != nil {
			return nil, errors.New("fetcher output contains more than one result")
		}
		found = parsed.FetchResult
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("failed to read fetcher output: %w", err)
	}
	if found == nil {
		return nil, errors.New("fetcher output contains no result")
	}

	return found, nil
}
