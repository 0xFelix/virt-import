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

package create_test

import (
	"context"
	"fmt"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"

	"kubevirt.io/virt-import-api/core/v1alpha1"
	importclient "kubevirt.io/virt-import-client-go/virtimport"
	importfake "kubevirt.io/virt-import-client-go/virtimport/fake"

	"kubevirt.io/virt-import/internal/virtimportctl/create"
	"kubevirt.io/virt-import/internal/virtimportctl/testing"
)

const (
	urlFlag           = "url"
	targetNameFlag    = "target-name"
	secretFlag        = "secret"
	certConfigMapFlag = "cert-configmap"
	storageClassFlag  = "storage-class"
	archFlag          = "arch"
)

var _ = Describe("Create command", func() {
	const (
		testName         = "fedora"
		testURL          = "docker://registry.example.com/vms/fedora:v1"
		testTargetName   = "fedora-vm"
		testSecret       = "registry-credentials" //nolint:gosec // A Secret name, not a credential.
		testCertCM       = "registry-ca"
		testStorageClass = "fast"
		testArch         = "arm64"
	)

	var importClient *importfake.Clientset

	BeforeEach(func() {
		// Ignore the kubeconfig of the machine running the tests.
		GinkgoT().Setenv("KUBECONFIG", filepath.Join(GinkgoT().TempDir(), "missing"))

		importClient = importfake.NewSimpleClientset()
		origGetImportClient := create.GetImportClient
		create.GetImportClient = func(_ *rest.Config) (importclient.Interface, error) {
			return importClient, nil
		}
		DeferCleanup(func() {
			create.GetImportClient = origGetImportClient
		})
	})

	getImport := func() *v1alpha1.VirtualMachineImport {
		vmImport, err := importClient.ImportV1alpha1().VirtualMachineImports(metav1.NamespaceDefault).
			Get(context.Background(), testName, metav1.GetOptions{})
		ExpectWithOffset(1, err).ToNot(HaveOccurred())
		return vmImport
	}

	Context("Creating VirtualMachineImport", func() {
		It("should create a VirtualMachineImport with the url only", func() {
			out, err := runCmd(testName, setFlag(urlFlag, testURL))
			Expect(err).ToNot(HaveOccurred())
			Expect(string(out)).To(Equal("virtualmachineimport.import.kubevirt.io/" + testName + " created\n"))

			vmImport := getImport()
			Expect(vmImport.Spec).To(Equal(v1alpha1.VirtualMachineImportSpec{
				Source: v1alpha1.VirtualMachineImportSource{
					Registry: &v1alpha1.VirtualMachineImportRegistrySource{URL: testURL},
				},
			}))
		})

		It("should pass all flags into the spec", func() {
			_, err := runCmd(testName,
				setFlag(urlFlag, testURL),
				setFlag(targetNameFlag, testTargetName),
				setFlag(secretFlag, testSecret),
				setFlag(certConfigMapFlag, testCertCM),
				setFlag(storageClassFlag, testStorageClass),
				setFlag(archFlag, testArch),
			)
			Expect(err).ToNot(HaveOccurred())

			Expect(getImport().Spec).To(Equal(v1alpha1.VirtualMachineImportSpec{
				Source: v1alpha1.VirtualMachineImportSource{
					Registry: &v1alpha1.VirtualMachineImportRegistrySource{
						URL:           testURL,
						SecretRef:     testSecret,
						CertConfigMap: testCertCM,
						Platform:      &v1alpha1.PlatformOptions{Architecture: testArch},
					},
				},
				TargetName:       testTargetName,
				StorageClassName: testStorageClass,
			}))
		})

		It("should create the VirtualMachineImport in the selected namespace", func() {
			const namespace = "production"
			_, err := runCmd(testName, setFlag(urlFlag, testURL), setFlag("namespace", namespace))
			Expect(err).ToNot(HaveOccurred())

			_, err = importClient.ImportV1alpha1().VirtualMachineImports(namespace).
				Get(context.Background(), testName, metav1.GetOptions{})
			Expect(err).ToNot(HaveOccurred())
		})
	})

	Context("Error handling", func() {
		It("should fail when --url is not provided", func() {
			_, err := runCmd(testName)
			Expect(err).To(MatchError(ContainSubstring("required flag(s) \"url\" not set")))
		})

		DescribeTable("should fail without exactly one name", func(args ...string) {
			_, err := runCmd(append(args, setFlag(urlFlag, testURL))...)
			Expect(err).To(MatchError(ContainSubstring("accepts 1 arg(s)")))
		},
			Entry("with none"),
			Entry("with two", testName, "other"),
		)

		It("should fail when the VirtualMachineImport exists", func() {
			_, err := runCmd(testName, setFlag(urlFlag, testURL))
			Expect(err).ToNot(HaveOccurred())
			_, err = runCmd(testName, setFlag(urlFlag, testURL))
			Expect(err).To(MatchError(ContainSubstring("error creating VirtualMachineImport")))
		})
	})
})

func setFlag(flag, parameter string) string {
	return fmt.Sprintf("--%s=%s", flag, parameter)
}

func runCmd(extraArgs ...string) ([]byte, error) {
	args := append([]string{"create"}, extraArgs...)
	return testing.NewRepeatableVirtctlCommandWithOut(args...)()
}
