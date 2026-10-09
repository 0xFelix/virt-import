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

package artifacts

import (
	"encoding/json"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"

	virtv1 "kubevirt.io/api/core/v1"
	cdiv1beta1 "kubevirt.io/containerized-data-importer-api/pkg/apis/core/v1beta1"
	templatev1beta1 "kubevirt.io/virt-template-api/core/v1beta1"

	"kubevirt.io/virt-import/internal/artifact"
)

// Fixture names, also the file names (plus .tar) in tests/artifacts.
const (
	VMSingle              = "vm-single"
	VMMultiDisk           = "vm-multidisk"
	VMMultiArch           = "vm-multiarch"
	VMPlainManifest       = "vm-plain-manifest"
	VMTemplateDVT         = "vmtemplate-dvt"
	BadArtifactType       = "bad-artifacttype"
	MissingSize           = "missing-size"
	DuplicateDisk         = "duplicate-disk"
	OrphanLayer           = "orphan-layer"
	UnlayeredPVC          = "unlayered-pvc"
	UnknownLayerMediaType = "unknown-layer-mediatype"
)

const (
	// DiskSize is the size of every fixture disk.
	DiskSize = 64 * 1024

	RootDisk = "rootdisk"
	DataDisk = "datadisk"

	ArchAMD64 = "amd64"
	ArchARM64 = "arm64"

	vmName       = "fixture-vm"
	templateName = "fixture-template"
	cloudInit    = "cloudinit"
	extraDisk    = "extradisk"
)

// Fixtures returns all fixtures by name.
func Fixtures() map[string]*Artifact {
	return map[string]*Artifact{
		VMSingle:    vmArtifact(ArchAMD64, []string{RootDisk}, RootDisk),
		VMMultiDisk: vmArtifact(ArchAMD64, []string{RootDisk, DataDisk}, RootDisk, DataDisk),
		VMMultiArch: {Images: []Image{
			vmImage(ArchAMD64, []string{RootDisk, DataDisk}, RootDisk, DataDisk),
			vmImage(ArchARM64, []string{RootDisk}, RootDisk),
		}},
		VMPlainManifest: {Images: []Image{vmImage(ArchAMD64, []string{RootDisk}, RootDisk)}, Plain: true},
		VMTemplateDVT: {Images: []Image{{
			Architecture:    ArchAMD64,
			ArtifactType:    artifact.ArtifactTypeVMTemplate,
			ConfigMediaType: artifact.MediaTypeVMTemplateConfig,
			Config:          templateConfig(),
			Disks:           []Disk{newDisk(RootDisk)},
		}}},
		BadArtifactType: {Images: []Image{{
			Architecture:    ArchAMD64,
			ArtifactType:    "application/vnd.example.v1",
			ConfigMediaType: "application/vnd.example.config.v1+json",
			Config:          []byte("{}"),
		}}},
		MissingSize: withDisks(vmArtifact(ArchAMD64, []string{RootDisk}), Disk{
			Name:        RootDisk,
			Content:     DiskContent(RootDisk, DiskSize),
			Annotations: map[string]string{artifact.AnnotationDiskName: RootDisk},
		}),
		DuplicateDisk: vmArtifact(ArchAMD64, []string{RootDisk}, RootDisk, RootDisk),
		OrphanLayer:   vmArtifact(ArchAMD64, []string{RootDisk}, RootDisk, extraDisk),
		UnlayeredPVC:  vmArtifact(ArchAMD64, []string{RootDisk, DataDisk}, RootDisk),
		UnknownLayerMediaType: withDisks(vmArtifact(ArchAMD64, []string{RootDisk}), Disk{
			Name:      RootDisk,
			Content:   DiskContent(RootDisk, DiskSize),
			MediaType: "application/vnd.kubevirt.persistentstate.tar+zstd",
		}),
	}
}

func vmArtifact(arch string, pvcVolumes []string, disks ...string) *Artifact {
	return &Artifact{Images: []Image{vmImage(arch, pvcVolumes, disks...)}}
}

func vmImage(arch string, pvcVolumes []string, disks ...string) Image {
	img := Image{
		Architecture:    arch,
		ArtifactType:    artifact.ArtifactTypeVM,
		ConfigMediaType: artifact.MediaTypeVMConfig,
	}
	var volumes []virtv1.Volume
	for _, name := range pvcVolumes {
		volumes = append(volumes, pvcVolume(vmName, name))
	}
	img.Config = mustJSON(vm(vmName, arch, volumes...))
	for _, name := range disks {
		img.Disks = append(img.Disks, newDisk(name))
	}
	return img
}

func withDisks(a *Artifact, disks ...Disk) *Artifact {
	a.Images[0].Disks = disks
	return a
}

func newDisk(name string) Disk {
	return Disk{Name: name, Content: DiskContent(name, DiskSize)}
}

func pvcVolume(vmName, name string) virtv1.Volume {
	return virtv1.Volume{
		Name: name,
		VolumeSource: virtv1.VolumeSource{
			PersistentVolumeClaim: &virtv1.PersistentVolumeClaimVolumeSource{
				PersistentVolumeClaimVolumeSource: corev1.PersistentVolumeClaimVolumeSource{ClaimName: vmName + "-" + name},
			},
		},
	}
}

// vm returns a halted VirtualMachine with the given volumes and a cloud-init volume.
func vm(name, arch string, volumes ...virtv1.Volume) *virtv1.VirtualMachine {
	volumes = append(volumes, virtv1.Volume{
		Name: cloudInit,
		VolumeSource: virtv1.VolumeSource{
			CloudInitNoCloud: &virtv1.CloudInitNoCloudSource{UserData: "#cloud-config\n"},
		},
	})
	disks := make([]virtv1.Disk, 0, len(volumes))
	for _, v := range volumes {
		disks = append(disks, virtv1.Disk{
			Name:       v.Name,
			DiskDevice: virtv1.DiskDevice{Disk: &virtv1.DiskTarget{Bus: virtv1.DiskBusVirtio}},
		})
	}

	gvk := virtv1.VirtualMachineGroupVersionKind
	return &virtv1.VirtualMachine{
		TypeMeta:   metav1.TypeMeta{APIVersion: gvk.GroupVersion().String(), Kind: gvk.Kind},
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: virtv1.VirtualMachineSpec{
			RunStrategy: ptr.To(virtv1.RunStrategyHalted),
			Template: &virtv1.VirtualMachineInstanceTemplateSpec{
				Spec: virtv1.VirtualMachineInstanceSpec{
					Architecture: arch,
					Domain:       virtv1.DomainSpec{Devices: virtv1.Devices{Disks: disks}},
					Volumes:      volumes,
				},
			},
		},
	}
}

func templateConfig() []byte {
	const dvtName = "${NAME}-" + RootDisk
	tplVM := vm("${NAME}", ArchAMD64, virtv1.Volume{
		Name:         RootDisk,
		VolumeSource: virtv1.VolumeSource{DataVolume: &virtv1.DataVolumeSource{Name: dvtName}},
	})
	tplVM.Spec.DataVolumeTemplates = []virtv1.DataVolumeTemplateSpec{{
		ObjectMeta: metav1.ObjectMeta{Name: dvtName},
		Spec: cdiv1beta1.DataVolumeSpec{
			Source: &cdiv1beta1.DataVolumeSource{PVC: &cdiv1beta1.DataVolumeSourcePVC{Name: templateName + "-" + RootDisk}},
			Storage: &cdiv1beta1.StorageSpec{Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: *resource.NewQuantity(DiskSize, resource.BinarySI)},
			}},
		},
	}}

	return mustJSON(&templatev1beta1.VirtualMachineTemplate{
		TypeMeta:   metav1.TypeMeta{APIVersion: templatev1beta1.GroupVersion.String(), Kind: "VirtualMachineTemplate"},
		ObjectMeta: metav1.ObjectMeta{Name: templateName},
		Spec: templatev1beta1.VirtualMachineTemplateSpec{
			Parameters:     []templatev1beta1.Parameter{{Name: "NAME", Required: true}},
			VirtualMachine: &runtime.RawExtension{Raw: mustJSON(tplVM)},
		},
	})
}

func mustJSON(v any) []byte {
	content, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return content
}
