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

// Copies of the unexported OCI artifact format constants in kubevirt/kubevirt
// pkg/storage/oci/oci.go (VEP #256). Replace with a shared package once one exists.
const (
	ArtifactTypeVM            = "application/vnd.kubevirt.virtualmachine.v1"
	MediaTypeVMConfig         = "application/vnd.kubevirt.virtualmachine.config.v1+json"
	ArtifactTypeVMTemplate    = "application/vnd.kubevirt.virtualmachinetemplate.v1"
	MediaTypeVMTemplateConfig = "application/vnd.kubevirt.virtualmachinetemplate.config.v1+json"
	MediaTypeDiskRawZstd      = "application/vnd.kubevirt.disk.raw+zstd"

	AnnotationDiskName = "io.kubevirt.disk.name"
	AnnotationDiskSize = "io.kubevirt.disk.size"

	// DefaultArchitecture is what the exporter records when the source does not set one.
	DefaultArchitecture = "amd64"
)
