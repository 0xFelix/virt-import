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

	"kubevirt.io/virt-import-api/core/v1alpha1"
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

type TargetOptions struct {
	Name      string
	Namespace string
	// ImportUID is the UID of the VirtualMachineImport, set as v1alpha1.LabelImportUID.
	ImportUID string
	// ClaimName returns the name of the PVC a disk is imported into.
	ClaimName func(diskName string) string
}

// Rewrite turns a config blob into the VirtualMachine or VirtualMachineTemplate to create:
// PVC volumes, and for templates DataVolumeTemplates cloning a PVC, are pointed at the
// imported PVCs. Every such volume must be named by exactly one disk and every disk must
// name one of them.
func Rewrite(artifactType string, config []byte, diskNames []string, opts *TargetOptions) (*unstructured.Unstructured, error) {
	obj, err := parseConfig(artifactType, config)
	if err != nil {
		return nil, err
	}
	if err := rewriteVolumes(obj, artifactType, diskNames, opts); err != nil {
		return nil, err
	}

	labels, annotations := obj.GetLabels(), obj.GetAnnotations()
	if labels == nil {
		labels = map[string]string{}
	}
	labels[v1alpha1.LabelImportUID] = opts.ImportUID
	// Labels and annotations are kept, the rest of the metadata belongs to the source object.
	unstructured.RemoveNestedField(obj.Object, "metadata")
	obj.SetName(opts.Name)
	obj.SetNamespace(opts.Namespace)
	obj.SetLabels(labels)
	obj.SetAnnotations(annotations)
	unstructured.RemoveNestedField(obj.Object, "status")

	return obj, nil
}

// ValidateConfig checks that a config blob can be rewritten for the given disks and returns
// the parsed config.
func ValidateConfig(artifactType string, config []byte, diskNames []string) (*unstructured.Unstructured, error) {
	obj, err := parseConfig(artifactType, config)
	if err != nil {
		return nil, err
	}
	if err := rewriteVolumes(obj, artifactType, diskNames, nil); err != nil {
		return nil, err
	}
	return obj, nil
}

func parseConfig(artifactType string, config []byte) (*unstructured.Unstructured, error) {
	gvk, ok := targetGVKs[artifactType]
	if !ok {
		return nil, fmt.Errorf("%w: unsupported artifactType %q", ErrInvalidArtifact, artifactType)
	}

	obj := &unstructured.Unstructured{}
	if err := obj.UnmarshalJSON(config); err != nil {
		return nil, fmt.Errorf("%w: config cannot be decoded: %w", ErrInvalidArtifact, err)
	}
	if obj.GroupVersionKind() != gvk {
		return nil, fmt.Errorf("%w: config holds %s, expected %s for artifactType %q",
			ErrInvalidArtifact, obj.GroupVersionKind(), gvk, artifactType)
	}

	return obj, nil
}

func vmPath(artifactType string, fields ...string) []string {
	return append(slices.Clone(vmPaths[artifactType]), fields...)
}

// rewriteVolumes points the PVC volumes at the imported PVCs, in place. With opts nil it only
// checks that volumes and disks match.
func rewriteVolumes(obj *unstructured.Unstructured, artifactType string, diskNames []string, opts *TargetOptions) error {
	disks := sets.New(diskNames...)
	named := sets.New[string]()

	volumes, err := nestedSliceNoCopy(obj.Object, vmPath(artifactType, fieldSpec, fieldTemplate, fieldSpec, "volumes")...)
	if err != nil {
		return fmt.Errorf("%w: invalid volumes: %w", ErrInvalidArtifact, err)
	}
	dvts, err := nestedSliceNoCopy(obj.Object, vmPath(artifactType, fieldSpec, "dataVolumeTemplates")...)
	if err != nil {
		return fmt.Errorf("%w: invalid dataVolumeTemplates: %w", ErrInvalidArtifact, err)
	}

	for _, v := range volumes {
		vol, ok := v.(map[string]any)
		if !ok {
			return fmt.Errorf("%w: invalid volume %v", ErrInvalidArtifact, v)
		}
		name, source, err := pvcSource(vol, artifactType, dvts)
		if err != nil {
			return err
		}
		if source == nil {
			continue
		}
		if !disks.Has(name) {
			return fmt.Errorf("%w: PVC volume %q is not named by any disk layer", ErrInvalidArtifact, name)
		}
		named.Insert(name)

		if opts == nil {
			continue
		}
		if _, isPVC := vol[fieldPVC]; isPVC {
			source["claimName"] = opts.ClaimName(name)
		} else {
			source[fieldName] = opts.ClaimName(name)
			source["namespace"] = opts.Namespace
		}
	}

	for _, name := range diskNames {
		if !named.Has(name) {
			return fmt.Errorf("%w: disk layer %q does not name a PVC volume", ErrInvalidArtifact, name)
		}
	}
	return nil
}

// nestedSliceNoCopy returns the slice at path, nil if there is none. Its elements are shared
// with obj, so changes to them end up in obj.
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

// pvcSource returns the name of a volume and the map referencing the PVC it is backed by: the
// persistentVolumeClaim of a PVC volume, or the spec.source.pvc of the DataVolumeTemplate a
// template's DataVolume volume clones from. The map is nil for any other volume.
func pvcSource(vol map[string]any, artifactType string, dvts []any) (name string, source map[string]any, err error) {
	name, _, _ = unstructured.NestedString(vol, fieldName)
	if v, isPVC := vol[fieldPVC]; isPVC {
		pvc, ok := v.(map[string]any)
		if !ok {
			return "", nil, fmt.Errorf("%w: volume %q has an invalid persistentVolumeClaim %v", ErrInvalidArtifact, name, v)
		}
		return name, pvc, nil
	}
	dvName, found, _ := unstructured.NestedString(vol, "dataVolume", fieldName)
	if !found {
		return name, nil, nil
	}
	if artifactType == ArtifactTypeVM {
		return "", nil, fmt.Errorf("%w: volume %q references DataVolume %q, a VirtualMachine artifact has PVC volumes only",
			ErrInvalidArtifact, name, dvName)
	}
	return name, findPVCCloneSource(dvts, dvName), nil
}

func findPVCCloneSource(dvts []any, dvName string) map[string]any {
	for _, d := range dvts {
		dvt, ok := d.(map[string]any)
		if !ok {
			continue
		}
		if name, _, _ := unstructured.NestedString(dvt, "metadata", fieldName); name != dvName {
			continue
		}
		spec, _ := dvt[fieldSpec].(map[string]any)
		source, _ := spec["source"].(map[string]any)
		pvc, _ := source["pvc"].(map[string]any)
		return pvc
	}
	return nil
}
