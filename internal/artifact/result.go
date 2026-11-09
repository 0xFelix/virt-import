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

// maxResultLineSize limits the length of the result line in the pod log.
const maxResultLineSize = 2 * 1024 * 1024

type FetchResult struct {
	ArtifactType string `json:"artifactType"`
	Architecture string `json:"architecture"`
	IsIndex      bool   `json:"isIndex"`
	// Digest is what the import is pinned to: the index digest if IsIndex, otherwise the manifest
	// digest.
	Digest digest.Digest `json:"digest"`
	// ManifestDigest is the digest of the selected manifest.
	ManifestDigest digest.Digest   `json:"manifestDigest"`
	Config         json.RawMessage `json:"config,omitempty"`
	Layers         []Layer         `json:"layers"`
}

type Layer struct {
	Digest   digest.Digest `json:"digest"`
	DiskName string        `json:"diskName"`
	DiskSize string        `json:"diskSize"`
}

// resultVersion is the version of the result line format, bump it on incompatible changes.
const resultVersion = 1

// resultLine is the envelope of the line the fetcher prints. Its fetchResult key tells it apart
// from other output in the pod log, like structured logs, so FetchResult has to stay first.
type resultLine struct {
	FetchResult *FetchResult `json:"fetchResult"`
	Version     int          `json:"version"`
}

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

func (r *FetchResult) DiskNames() []string {
	names := make([]string, 0, len(r.Layers))
	for _, layer := range r.Layers {
		names = append(names, layer.DiskName)
	}
	return names
}

// resultPrefix starts every line WriteResult writes, telling it apart from logs and panics.
const resultPrefix = `{"fetchResult":`

// WriteResult writes result as a single {"fetchResult":...} line. A result over
// maxResultLineSize is an invalid artifact.
func WriteResult(w io.Writer, result *FetchResult) error {
	buf := &bytes.Buffer{}
	enc := json.NewEncoder(buf)
	// Escaping <, > and & would let the config grow up to six times beyond its size limit.
	enc.SetEscapeHTML(false)
	if err := enc.Encode(resultLine{FetchResult: result, Version: resultVersion}); err != nil {
		return err
	}
	if buf.Len() > maxResultLineSize {
		return InvalidArtifactf("the fetch result is %d bytes, at most %d are supported", buf.Len(), maxResultLineSize)
	}
	_, err := w.Write(buf.Bytes())
	return err
}

// ParseResult finds the single line WriteResult wrote in the fetcher output. Every other line,
// like a log line or a panic, is ignored.
func ParseResult(r io.Reader) (*FetchResult, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, bufio.MaxScanTokenSize), maxResultLineSize)

	var found *FetchResult
	for scanner.Scan() {
		line := scanner.Bytes()
		if !bytes.HasPrefix(line, []byte(resultPrefix)) {
			continue
		}
		parsed := resultLine{}
		if err := json.Unmarshal(line, &parsed); err != nil {
			return nil, fmt.Errorf("invalid result line: %w", err)
		}
		if parsed.FetchResult == nil {
			return nil, errors.New("the result line holds no result")
		}
		if parsed.Version != resultVersion {
			return nil, fmt.Errorf("unsupported result version %d, expected %d", parsed.Version, resultVersion)
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
