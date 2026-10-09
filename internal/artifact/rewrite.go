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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	virtv1 "kubevirt.io/api/core/v1"
	templatev1beta1 "kubevirt.io/virt-template-api/core/v1beta1"

	"kubevirt.io/virt-import-api/core/v1alpha1"
)

const (
	kindVirtualMachineTemplate = "VirtualMachineTemplate"

	fieldSpec           = "spec"
	fieldVirtualMachine = "virtualMachine"
	fieldName           = "name"
	fieldPVC            = "persistentVolumeClaim"
)

var (
	vmVolumesPath           = []string{fieldSpec, "template", fieldSpec, "volumes"}
	templateVMPath          = []string{fieldSpec, fieldVirtualMachine}
	dataVolumeTemplatesPath = append(slices.Clone(templateVMPath), fieldSpec, "dataVolumeTemplates")
)

var targetGVKs = map[string]schema.GroupVersionKind{
	ArtifactTypeVM:         virtv1.VirtualMachineGroupVersionKind,
	ArtifactTypeVMTemplate: templatev1beta1.GroupVersion.WithKind(kindVirtualMachineTemplate),
}

var volumesPaths = map[string][]string{
	ArtifactTypeVM:         vmVolumesPath,
	ArtifactTypeVMTemplate: append(slices.Clone(templateVMPath), vmVolumesPath...),
}

// TargetOptions describe the object a config blob is rewritten into.
type TargetOptions struct {
	// Name and Namespace of the target.
	Name, Namespace string
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
	if err := rewriteVolumes(obj, artifactType, diskNames, opts.ClaimName, opts.Namespace); err != nil {
		return nil, err
	}

	obj.SetName(opts.Name)
	obj.SetNamespace(opts.Namespace)
	obj.SetGenerateName("")
	obj.SetUID("")
	obj.SetResourceVersion("")
	obj.SetGeneration(0)
	obj.SetCreationTimestamp(metav1.Time{})
	obj.SetDeletionTimestamp(nil)
	obj.SetDeletionGracePeriodSeconds(nil)
	obj.SetManagedFields(nil)
	obj.SetOwnerReferences(nil)
	obj.SetFinalizers(nil)
	labels := obj.GetLabels()
	if labels == nil {
		labels = map[string]string{}
	}
	labels[v1alpha1.LabelImportUID] = opts.ImportUID
	obj.SetLabels(labels)
	unstructured.RemoveNestedField(obj.Object, "status")

	return obj, nil
}

// ValidateConfig checks that a config blob can be rewritten for the given disks.
func ValidateConfig(artifactType string, config []byte, diskNames []string) error {
	obj, err := parseConfig(artifactType, config)
	if err != nil {
		return err
	}
	return rewriteVolumes(obj, artifactType, diskNames, func(string) string { return "" }, "")
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

func rewriteVolumes(
	obj *unstructured.Unstructured, artifactType string, diskNames []string,
	claimName func(string) string, namespace string,
) error {
	disks := map[string]bool{}
	for _, name := range diskNames {
		disks[name] = false
	}

	volumesPath := volumesPaths[artifactType]
	volumes, dvts, err := volumesAndDataVolumeTemplates(obj, artifactType)
	if err != nil {
		return err
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
		if _, ok := disks[name]; !ok {
			return fmt.Errorf("%w: PVC volume %q is not named by any disk layer", ErrInvalidArtifact, name)
		}
		disks[name] = true

		if _, isPVC := vol[fieldPVC]; isPVC {
			source["claimName"] = claimName(name)
		} else {
			source[fieldName] = claimName(name)
			source["namespace"] = namespace
		}
	}

	for _, name := range diskNames {
		if !disks[name] {
			return fmt.Errorf("%w: disk layer %q does not name a PVC volume", ErrInvalidArtifact, name)
		}
	}

	if volumes != nil {
		if err := unstructured.SetNestedSlice(obj.Object, volumes, volumesPath...); err != nil {
			return err
		}
	}
	if dvts != nil {
		return unstructured.SetNestedSlice(obj.Object, dvts, dataVolumeTemplatesPath...)
	}
	return nil
}

// volumesAndDataVolumeTemplates returns copies of the volumes and, for templates, the
// DataVolumeTemplates of the VirtualMachine in obj.
func volumesAndDataVolumeTemplates(obj *unstructured.Unstructured, artifactType string) (volumes, dvts []any, err error) {
	volumes, _, err = unstructured.NestedSlice(obj.Object, volumesPaths[artifactType]...)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: invalid volumes: %w", ErrInvalidArtifact, err)
	}
	if artifactType == ArtifactTypeVMTemplate {
		if dvts, _, err = unstructured.NestedSlice(obj.Object, dataVolumeTemplatesPath...); err != nil {
			return nil, nil, fmt.Errorf("%w: invalid dataVolumeTemplates: %w", ErrInvalidArtifact, err)
		}
	}
	return volumes, dvts, nil
}

// pvcSource returns the name of a volume and the map referencing the PVC it is backed by: the
// persistentVolumeClaim of a PVC volume, or the spec.source.pvc of the DataVolumeTemplate a
// template's DataVolume volume clones from. The map is nil for any other volume.
func pvcSource(vol map[string]any, artifactType string, dvts []any) (name string, source map[string]any, err error) {
	name, _, _ = unstructured.NestedString(vol, fieldName)
	if pvc, isPVC := vol[fieldPVC].(map[string]any); isPVC {
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

// findPVCCloneSource returns the spec.source.pvc of the DataVolumeTemplate named dvName, if it
// clones a PVC. The map is part of dvts, so changes to it end up in dvts.
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
