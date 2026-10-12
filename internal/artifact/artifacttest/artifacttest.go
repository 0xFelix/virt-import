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

// Package artifacttest builds OCI artifacts in the VEP #256 export format for tests.
package artifacttest

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"github.com/klauspost/compress/zstd"
	"github.com/opencontainers/go-digest"
	"github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"kubevirt.io/virt-import/internal/artifact"
)

const (
	created     = "2026-01-01T00:00:00Z"
	fileMode    = 0o644
	dirMode     = 0o755
	titleSuffix = ".raw.zst"

	diskContentBlock = 1024
)

type Disk struct {
	// Name is the io.kubevirt.disk.name annotation.
	Name string
	// Content is the uncompressed disk.
	Content []byte
	// MediaType overrides artifact.MediaTypeDiskRawZstd.
	MediaType string
	// Annotations replace the default layer annotations when not nil.
	Annotations map[string]string
}

type Image struct {
	// Architecture is the platform architecture recorded in the index.
	Architecture                  string
	ArtifactType, ConfigMediaType string
	Config                        []byte
	Disks                         []Disk
}

type Artifact struct {
	Images []Image
	// Plain stores the only image as a plain manifest without an index.
	Plain bool
	// Algorithm digests the blobs, digest.Canonical if empty.
	Algorithm digest.Algorithm
}

type blob struct {
	digest  digest.Digest
	content []byte
}

type layout struct {
	algorithm digest.Algorithm
	blobs     []blob
	// top is the descriptor of the image index, or of the manifest of a plain artifact.
	top ocispec.Descriptor
}

func (a *Artifact) WriteLayout(dir string) error {
	l, err := a.build()
	if err != nil {
		return err
	}

	layoutJSON, err := json.Marshal(ocispec.ImageLayout{Version: ocispec.ImageLayoutVersion})
	if err != nil {
		return err
	}
	indexJSON, err := json.Marshal(ocispec.Index{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: ocispec.MediaTypeImageIndex,
		Manifests: []ocispec.Descriptor{l.top},
	})
	if err != nil {
		return err
	}

	files := map[string][]byte{
		ocispec.ImageLayoutFile: layoutJSON,
		ocispec.ImageIndexFile:  indexJSON,
	}
	for _, b := range l.blobs {
		files[filepath.Join(ocispec.ImageBlobsDir, b.digest.Algorithm().String(), b.digest.Encoded())] = b.content
	}
	for name, content := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), dirMode); err != nil {
			return err
		}
		if err := os.WriteFile(path, content, fileMode); err != nil {
			return err
		}
	}
	return nil
}

// DiskContent returns size bytes of content unique to the disk name. It repeats a pseudo-random
// block, so it compresses well but stays above the 512 byte header CDI reads to detect the format.
func DiskContent(name string, size int) []byte {
	block := make([]byte, 0, diskContentBlock)
	sum := sha256.Sum256([]byte(name))
	for len(block) < diskContentBlock {
		block = append(block, sum[:]...)
		sum = sha256.Sum256(sum[:])
	}
	return bytes.Repeat(block, size/diskContentBlock+1)[:size]
}

func (a *Artifact) build() (*layout, error) {
	if a.Plain && len(a.Images) != 1 {
		return nil, fmt.Errorf("a plain artifact needs exactly one image, got %d", len(a.Images))
	}

	l := &layout{algorithm: a.Algorithm}
	if l.algorithm == "" {
		l.algorithm = digest.Canonical
	}
	descs := make([]ocispec.Descriptor, 0, len(a.Images))
	for i := range a.Images {
		desc, err := l.addImage(&a.Images[i])
		if err != nil {
			return nil, err
		}
		descs = append(descs, desc)
	}

	top := descs[0]
	if !a.Plain {
		index := ocispec.Index{
			Versioned:    specs.Versioned{SchemaVersion: 2},
			MediaType:    ocispec.MediaTypeImageIndex,
			ArtifactType: a.Images[0].ArtifactType,
			Manifests:    descs,
			Annotations:  map[string]string{ocispec.AnnotationCreated: created},
		}
		var err error
		if top, err = l.addJSON(ocispec.MediaTypeImageIndex, index); err != nil {
			return nil, err
		}
	}
	top.Platform = nil
	l.top = top

	return l, nil
}

func (l *layout) addImage(img *Image) (ocispec.Descriptor, error) {
	config := l.addBlob(img.ConfigMediaType, img.Config)

	layers := make([]ocispec.Descriptor, 0, len(img.Disks))
	for _, disk := range img.Disks {
		layer, err := l.addDisk(&disk)
		if err != nil {
			return ocispec.Descriptor{}, err
		}
		layers = append(layers, layer)
	}

	desc, err := l.addJSON(ocispec.MediaTypeImageManifest, ocispec.Manifest{
		Versioned:    specs.Versioned{SchemaVersion: 2},
		MediaType:    ocispec.MediaTypeImageManifest,
		ArtifactType: img.ArtifactType,
		Config:       config,
		Layers:       layers,
	})
	if err != nil {
		return ocispec.Descriptor{}, err
	}
	desc.ArtifactType = img.ArtifactType
	desc.Platform = &ocispec.Platform{Architecture: img.Architecture, OS: artifact.OS}

	return desc, nil
}

func (l *layout) addDisk(disk *Disk) (ocispec.Descriptor, error) {
	encoder, err := zstd.NewWriter(nil, zstd.WithEncoderConcurrency(1))
	if err != nil {
		return ocispec.Descriptor{}, err
	}
	defer encoder.Close()

	mediaType := disk.MediaType
	if mediaType == "" {
		mediaType = artifact.MediaTypeDiskRawZstd
	}
	desc := l.addBlob(mediaType, encoder.EncodeAll(disk.Content, nil))

	desc.Annotations = disk.Annotations
	if desc.Annotations == nil {
		desc.Annotations = map[string]string{
			artifact.AnnotationDiskName: disk.Name,
			artifact.AnnotationDiskSize: resource.NewQuantity(int64(len(disk.Content)), resource.BinarySI).String(),
			ocispec.AnnotationTitle:     disk.Name + titleSuffix,
		}
	}

	return desc, nil
}

func (l *layout) addJSON(mediaType string, v any) (ocispec.Descriptor, error) {
	content, err := json.Marshal(v)
	if err != nil {
		return ocispec.Descriptor{}, err
	}
	return l.addBlob(mediaType, content), nil
}

func (l *layout) addBlob(mediaType string, content []byte) ocispec.Descriptor {
	d := l.algorithm.FromBytes(content)
	if !slices.ContainsFunc(l.blobs, func(b blob) bool { return b.digest == d }) {
		l.blobs = append(l.blobs, blob{digest: d, content: content})
	}

	return ocispec.Descriptor{MediaType: mediaType, Digest: d, Size: int64(len(content))}
}
