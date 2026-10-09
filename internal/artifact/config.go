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

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/sets"

	virtv1 "kubevirt.io/api/core/v1"
	templatev1beta1 "kubevirt.io/virt-template-api/core/v1beta1"
)

const (
	kindVirtualMachineTemplate = "VirtualMachineTemplate"

	fieldSpec     = "spec"
	fieldTemplate = "template"
	fieldName     = "name"
	fieldPVC      = "persistentVolumeClaim"
)

var (
	// vmPaths locate the VirtualMachine in a config blob.
	vmPaths = map[string][]string{
		ArtifactTypeVM:         nil,
		ArtifactTypeVMTemplate: {fieldSpec, "virtualMachine"},
	}
	targetGVKs = map[string]schema.GroupVersionKind{
		ArtifactTypeVM:         virtv1.VirtualMachineGroupVersionKind,
		ArtifactTypeVMTemplate: templatev1beta1.GroupVersion.WithKind(kindVirtualMachineTemplate),
	}
)

// ValidateConfig parses a config blob and checks that its PVC volumes and the disk layers match
// one to one by name.
func ValidateConfig(artifactType string, config []byte, diskNames []string) (*unstructured.Unstructured, error) {
	obj, err := parseConfig(artifactType, config)
	if err != nil {
		return nil, err
	}
	if _, err := validateVolumes(obj, artifactType, diskNames); err != nil {
		return nil, err
	}
	return obj, nil
}

func parseConfig(artifactType string, config []byte) (*unstructured.Unstructured, error) {
	gvk, ok := targetGVKs[artifactType]
	if !ok {
		return nil, InvalidArtifactf("unsupported artifactType %q", artifactType)
	}

	obj := &unstructured.Unstructured{}
	if err := obj.UnmarshalJSON(config); err != nil {
		return nil, InvalidArtifactf("config cannot be decoded: %w", err)
	}
	if obj.GroupVersionKind() != gvk {
		return nil, InvalidArtifactf("config holds %s, expected %s for artifactType %q",
			obj.GroupVersionKind(), gvk, artifactType)
	}

	return obj, nil
}

func vmPath(artifactType string, fields ...string) []string {
	return append(slices.Clone(vmPaths[artifactType]), fields...)
}

// pvcRef points to the PVC behind a volume.
type pvcRef struct {
	volumeName string
	// source is part of the parsed config, not a copy.
	source map[string]any
	// isPVC is true for a persistentVolumeClaim volume, false for the PVC clone source of a
	// DataVolumeTemplate.
	isPVC bool
}

// validateVolumes checks that the PVC volumes and the disk layers match one to one by name, and
// returns the PVCs.
func validateVolumes(obj *unstructured.Unstructured, artifactType string, diskNames []string) ([]pvcRef, error) {
	disks := sets.New(diskNames...)
	named := sets.New[string]()

	volumes, err := nestedSliceNoCopy(obj.Object, vmPath(artifactType, fieldSpec, fieldTemplate, fieldSpec, "volumes")...)
	if err != nil {
		return nil, InvalidArtifactf("invalid volumes: %w", err)
	}
	dvts, err := nestedSliceNoCopy(obj.Object, vmPath(artifactType, fieldSpec, "dataVolumeTemplates")...)
	if err != nil {
		return nil, InvalidArtifactf("invalid dataVolumeTemplates: %w", err)
	}

	var refs []pvcRef
	for _, v := range volumes {
		vol, ok := v.(map[string]any)
		if !ok {
			return nil, InvalidArtifactf("invalid volume %v", v)
		}
		ref, err := pvcSource(vol, artifactType, dvts)
		if err != nil {
			return nil, err
		}
		if ref == nil {
			continue
		}
		if !disks.Has(ref.volumeName) {
			return nil, InvalidArtifactf("PVC volume %q is not named by any disk layer", ref.volumeName)
		}
		named.Insert(ref.volumeName)
		refs = append(refs, *ref)
	}

	for _, name := range diskNames {
		if !named.Has(name) {
			return nil, InvalidArtifactf("disk layer %q does not name a PVC volume", name)
		}
	}
	return refs, nil
}

// nestedSliceNoCopy is unstructured.NestedSlice without the deep copy.
func nestedSliceNoCopy(obj map[string]any, path ...string) ([]any, error) {
	v, found, err := unstructured.NestedFieldNoCopy(obj, path...)
	if err != nil || !found {
		return nil, err
	}
	s, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("%s is of the type %T, expected a list", strings.Join(path, "."), v)
	}
	return s, nil
}

// pvcSource returns the PVC behind a volume: its persistentVolumeClaim, or in a template the PVC
// its DataVolumeTemplate clones. It returns nil for other volumes.
func pvcSource(vol map[string]any, artifactType string, dvts []any) (*pvcRef, error) {
	name, _, _ := unstructured.NestedString(vol, fieldName)
	if dvName, isDV, _ := unstructured.NestedString(vol, "dataVolume", fieldName); isDV {
		if artifactType == ArtifactTypeVM {
			return nil, InvalidArtifactf("volume %q references DataVolume %q, a VirtualMachine artifact has PVC volumes only",
				name, dvName)
		}
		source, err := findPVCCloneSource(dvts, dvName)
		if err != nil || source == nil {
			return nil, err
		}
		return &pvcRef{volumeName: name, source: source}, nil
	}

	v, isPVC := vol[fieldPVC]
	if !isPVC {
		return nil, nil
	}
	pvc, ok := v.(map[string]any)
	if !ok {
		return nil, InvalidArtifactf("volume %q has an invalid persistentVolumeClaim %v", name, v)
	}
	return &pvcRef{volumeName: name, source: pvc, isPVC: true}, nil
}

// findPVCCloneSource returns the spec.source.pvc of the DataVolumeTemplate named dvName, nil if
// it has none or does not exist.
func findPVCCloneSource(dvts []any, dvName string) (map[string]any, error) {
	for _, d := range dvts {
		dvt, ok := d.(map[string]any)
		if !ok {
			return nil, InvalidArtifactf("invalid dataVolumeTemplate %v", d)
		}
		if name, _, _ := unstructured.NestedString(dvt, "metadata", fieldName); name != dvName {
			continue
		}
		v, found, err := unstructured.NestedFieldNoCopy(dvt, fieldSpec, "source", "pvc")
		if err != nil {
			return nil, InvalidArtifactf("dataVolumeTemplate %q has an invalid source: %w", dvName, err)
		}
		if !found {
			return nil, nil
		}
		pvc, ok := v.(map[string]any)
		if !ok {
			return nil, InvalidArtifactf("dataVolumeTemplate %q has an invalid source.pvc %v", dvName, v)
		}
		return pvc, nil
	}
	return nil, nil
}
