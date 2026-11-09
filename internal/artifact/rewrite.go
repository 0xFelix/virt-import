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
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"kubevirt.io/virt-import-api/core/v1alpha1"
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
// imported PVCs. The config has to pass ValidateConfig for the same disks.
func Rewrite(artifactType string, config []byte, diskNames []string, opts *TargetOptions) (*unstructured.Unstructured, error) {
	obj, err := parseConfig(artifactType, config)
	if err != nil {
		return nil, err
	}
	refs, err := validateVolumes(obj, artifactType, diskNames)
	if err != nil {
		return nil, err
	}
	rewriteVolumes(refs, opts)

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

// TargetGVK returns the kind of the target an artifact type is imported as.
func TargetGVK(artifactType string) (schema.GroupVersionKind, bool) {
	gvk, ok := targetGVKs[artifactType]
	return gvk, ok
}

// rewriteVolumes points the PVC references at the imported PVCs.
func rewriteVolumes(refs []pvcRef, opts *TargetOptions) {
	for _, ref := range refs {
		if ref.isPVC {
			ref.source["claimName"] = opts.ClaimName(ref.volumeName)
		} else {
			ref.source[fieldName] = opts.ClaimName(ref.volumeName)
			ref.source["namespace"] = opts.Namespace
		}
	}
}
