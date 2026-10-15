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

package tests_test

import (
	"context"
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/utils/ptr"

	virtv1 "kubevirt.io/api/core/v1"
	exportv1 "kubevirt.io/api/export/v1"

	"kubevirt.io/virt-import/internal/artifact/artifacttest"
)

// These specs import artifacts made by KubeVirt's OCI export. The exported objects are created by
// importing fixtures, which gives disks with known contents. They need KubeVirt with the OCIExport
// feature gate and a disk layer annotation holding the volume name (kubevirt/kubevirt#19066).
var _ = Describe("VirtualMachineImport of a KubeVirt export", Label("KubeVirtExport"), func() {
	It("should import an exported VirtualMachine", func() {
		source := waitForImportReady(createImport(fixtureURL(artifacttest.VMMultiDisk)).Name)

		url := exportToRegistry(virtv1.VirtualMachineGroupVersionKind, source.Name)
		imported := waitForImportReady(createImport(url).Name)

		vm, err := virtClient.KubevirtV1().VirtualMachines(testNamespace).Get(context.Background(), imported.Name, metav1.GetOptions{})
		Expect(err).ToNot(HaveOccurred())
		sourceVM, err := virtClient.KubevirtV1().VirtualMachines(testNamespace).Get(context.Background(), source.Name, metav1.GetOptions{})
		Expect(err).ToNot(HaveOccurred())
		Expect(vm.Spec.Template.Spec.Domain.Devices.Disks).To(Equal(sourceVM.Spec.Template.Spec.Domain.Devices.Disks))
		Expect(claimNames(vm)).To(HaveLen(2))

		for _, disk := range []string{artifacttest.RootDisk, artifacttest.DataDisk} {
			Expect(claimNames(vm)).To(HaveKeyWithValue(disk, dataVolumeNameOf(imported, disk)))
			Expect(diskChecksum(testNamespace, dataVolumeNameOf(imported, disk))).To(Equal(expectedChecksum(disk)))
		}
	})

	It("should import an exported VirtualMachineTemplate", func() {
		source := waitForImportReady(createImport(fixtureURL(artifacttest.VMTemplateDVT)).Name)

		url := exportToRegistry(templateGVK, source.Name)
		imported := waitForImportReady(createImport(url).Name)
		Expect(imported.Status.TargetRef.Kind).To(Equal(templateGVK.Kind))

		pvcName := dataVolumeNameOf(imported, artifacttest.RootDisk)
		Expect(templateVM(getTemplate(imported.Name)).Spec.DataVolumeTemplates).To(ConsistOf(clonePVCSource(pvcName)))
		Expect(diskChecksum(testNamespace, pvcName)).To(Equal(expectedChecksum(artifacttest.RootDisk)))
	})
})

// exportToRegistry exports a VirtualMachine or VirtualMachineTemplate as an OCI artifact, pushes
// it to the test registry from inside the cluster and returns its docker:// URL.
func exportToRegistry(gvk schema.GroupVersionKind, name string) string {
	GinkgoHelper()

	vmExport, err := virtClient.ExportV1().VirtualMachineExports(testNamespace).Create(context.Background(),
		&exportv1.VirtualMachineExport{
			ObjectMeta: metav1.ObjectMeta{GenerateName: "export-"},
			Spec: exportv1.VirtualMachineExportSpec{
				Source: corev1.TypedLocalObjectReference{APIGroup: ptr.To(gvk.Group), Kind: gvk.Kind, Name: name},
			},
		}, metav1.CreateOptions{})
	Expect(err).ToNot(HaveOccurred())

	var ociURL, cert string
	Eventually(func(g Gomega) {
		current, err := virtClient.ExportV1().VirtualMachineExports(testNamespace).Get(context.Background(), vmExport.Name, metav1.GetOptions{})
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(current.Status).ToNot(BeNil())
		g.Expect(current.Status.Phase).To(Equal(exportv1.Ready), "export status: %+v", current.Status)
		g.Expect(current.Status.TokenSecretRef).ToNot(BeNil())
		g.Expect(current.Status.Links).ToNot(BeNil())
		g.Expect(current.Status.Links.Internal).ToNot(BeNil())
		for _, manifest := range current.Status.Links.Internal.Manifests {
			if manifest.Type == exportv1.OCI {
				ociURL = manifest.Url
			}
		}
		g.Expect(ociURL).ToNot(BeEmpty(), "export offers no OCI manifest: %+v", current.Status.Links.Internal)
		cert = current.Status.Links.Internal.Cert
		vmExport = current
	}, importTimeout, pollInterval).Should(Succeed())

	url := fmt.Sprintf("docker://%s/%sexport-%s:%s", *testRegistry, testRepositoryPrefix, rand.String(8), testTag)
	// The archive is unpacked with tar because skopeo's oci-archive transport chowns the files it
	// extracts, which fails as non-root.
	script := fmt.Sprintf(`set -eo pipefail
printf '%%s' "$CERT" > /tmp/ca.crt
mkdir /tmp/layout
curl --fail --silent --show-error --cacert /tmp/ca.crt -H "x-kubevirt-export-token: $TOKEN" "$OCI_URL" |
  tar xf - -C /tmp/layout --no-same-owner
skopeo copy --quiet --multi-arch=all --dest-tls-verify=false oci:/tmp/layout %s
`, url)
	runPod(testNamespace, &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "export-push-"},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			Containers: []corev1.Container{{
				Name:    "push",
				Image:   *testImage,
				Command: shellCommand(script),
				Env: []corev1.EnvVar{
					{Name: "OCI_URL", Value: ociURL},
					{Name: "CERT", Value: cert},
					{Name: "TOKEN", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{Name: *vmExport.Status.TokenSecretRef},
						Key:                  "token",
					}}},
				},
			}},
		},
	})

	return url
}
