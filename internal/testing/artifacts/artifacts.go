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

// Package artifacts builds OCI artifacts in the VEP #256 export format for tests.
package artifacts

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/opencontainers/go-digest"
	"github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"kubevirt.io/virt-import/internal/artifact"
)

const (
	osLinux       = "linux"
	created       = "2026-01-01T00:00:00Z"
	fileMode      = 0o644
	dirMode       = 0o755
	blobsDir      = "blobs"
	titleSuffix   = ".raw.zst"
	sha256Subpath = "sha256"
)

// Disk is a disk layer.
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

// Image is a single manifest with its config and disks.
type Image struct {
	// Architecture is the platform architecture recorded in the index.
	Architecture string
	// ArtifactType and ConfigMediaType of the manifest.
	ArtifactType, ConfigMediaType string
	// Config is the config blob.
	Config []byte
	// Disks are the disk layers.
	Disks []Disk
}

// Artifact is an OCI artifact made of one or more images.
type Artifact struct {
	// Images are referenced by an image index, like the exporter writes them.
	Images []Image
	// Plain stores the only image as a plain manifest without an index.
	Plain bool
}

type blob struct {
	digest  digest.Digest
	content []byte
}

// layout is an OCI image layout held in memory.
type layout struct {
	indexJSON []byte
	blobs     []blob
}

// WriteArchive writes the artifact as an oci-archive tarball.
func (a *Artifact) WriteArchive(w io.Writer) error {
	l, err := a.build()
	if err != nil {
		return err
	}

	tw := tar.NewWriter(w)
	if err := writeTarFile(tw, ocispec.ImageLayoutFile, layoutFile()); err != nil {
		return err
	}
	if err := writeTarFile(tw, ocispec.ImageIndexFile, l.indexJSON); err != nil {
		return err
	}
	if err := writeTarDir(tw, blobsDir+"/"); err != nil {
		return err
	}
	if err := writeTarDir(tw, blobsDir+"/"+sha256Subpath+"/"); err != nil {
		return err
	}
	for _, b := range l.blobs {
		if err := writeTarFile(tw, blobPath(b.digest), b.content); err != nil {
			return err
		}
	}
	return tw.Close()
}

// WriteLayout writes the artifact as an OCI image layout into dir.
func (a *Artifact) WriteLayout(dir string) error {
	l, err := a.build()
	if err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Join(dir, blobsDir, sha256Subpath), dirMode); err != nil {
		return err
	}
	files := map[string][]byte{
		ocispec.ImageLayoutFile: layoutFile(),
		ocispec.ImageIndexFile:  l.indexJSON,
	}
	for _, b := range l.blobs {
		files[blobPath(b.digest)] = b.content
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), content, fileMode); err != nil {
			return err
		}
	}
	return nil
}

// DiskContent returns size bytes of reproducible content for the disk name: the SHA-256 of the
// name, repeated. It differs per disk and compresses well, which keeps the fixtures small.
func DiskContent(name string, size int) []byte {
	sum := sha256.Sum256([]byte(name))
	return bytes.Repeat(sum[:], size/len(sum)+1)[:size]
}

func (a *Artifact) build() (*layout, error) {
	if a.Plain && len(a.Images) != 1 {
		return nil, fmt.Errorf("a plain artifact needs exactly one image, got %d", len(a.Images))
	}

	l := &layout{}
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

	indexJSON, err := json.Marshal(ocispec.Index{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: ocispec.MediaTypeImageIndex,
		Manifests: []ocispec.Descriptor{top},
	})
	if err != nil {
		return nil, err
	}
	l.indexJSON = indexJSON

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
	desc.Platform = &ocispec.Platform{Architecture: img.Architecture, OS: osLinux}

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
	d := digest.FromBytes(content)
	known := false
	for _, b := range l.blobs {
		known = known || b.digest == d
	}
	if !known {
		l.blobs = append(l.blobs, blob{digest: d, content: content})
	}

	return ocispec.Descriptor{MediaType: mediaType, Digest: d, Size: int64(len(content))}
}

func layoutFile() []byte {
	return []byte(`{"imageLayoutVersion":"` + ocispec.ImageLayoutVersion + `"}`)
}

func blobPath(d digest.Digest) string {
	return blobsDir + "/" + d.Algorithm().String() + "/" + d.Encoded()
}

func writeTarDir(tw *tar.Writer, name string) error {
	return tw.WriteHeader(&tar.Header{
		Typeflag: tar.TypeDir,
		Name:     name,
		Mode:     dirMode,
		ModTime:  time.Unix(0, 0),
		Format:   tar.FormatUSTAR,
	})
}

func writeTarFile(tw *tar.Writer, name string, content []byte) error {
	if err := tw.WriteHeader(&tar.Header{
		Typeflag: tar.TypeReg,
		Name:     name,
		Size:     int64(len(content)),
		Mode:     fileMode,
		ModTime:  time.Unix(0, 0),
		Format:   tar.FormatUSTAR,
	}); err != nil {
		return err
	}
	_, err := tw.Write(content)
	return err
}
