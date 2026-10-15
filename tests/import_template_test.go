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
	"encoding/json"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	gomegatypes "github.com/onsi/gomega/types"

	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/rand"

	virtv1 "kubevirt.io/api/core/v1"
	cdiv1beta1 "kubevirt.io/containerized-data-importer-api/pkg/apis/core/v1beta1"

	"kubevirt.io/virt-template-api/core/subresourcesv1beta1"
	templatev1beta1 "kubevirt.io/virt-template-api/core/v1beta1"

	"kubevirt.io/virt-import-api/core/v1alpha1"

	"kubevirt.io/virt-import/internal/artifact/artifacttest"
)

var _ = Describe("VirtualMachineImport of a VirtualMachineTemplate", func() {
	It("should import a VirtualMachineTemplate", func() {
		vmImport := waitForImportReady(createImport(fixtureURL(artifacttest.VMTemplateDVT)).Name)
		Expect(vmImport.Status.TargetRef.Kind).To(Equal(templateGVK.Kind))

		tpl := getTemplate(vmImport.Name)
		Expect(tpl.Labels).To(HaveKeyWithValue(v1alpha1.LabelImportUID, string(vmImport.UID)))
		pvcName := dataVolumeNameOf(vmImport, artifacttest.RootDisk)
		Expect(templateVM(tpl).Spec.DataVolumeTemplates).To(ConsistOf(clonePVCSource(pvcName)))
		Expect(diskChecksum(testNamespace, pvcName)).To(Equal(expectedChecksum(artifacttest.RootDisk)))
	})

	It("should import a VirtualMachineTemplate with several kinds of volumes", func() {
		vmImport := waitForImportReady(createImport(fixtureURL(artifacttest.VMTemplateMultiDisk)).Name)
		Expect(vmImport.Status.DiskImports).To(HaveLen(2))
		rootPVC := dataVolumeNameOf(vmImport, artifacttest.RootDisk)
		dataPVC := dataVolumeNameOf(vmImport, artifacttest.DataDisk)

		vm := templateVM(getTemplate(vmImport.Name))
		By("pointing the DataVolumeTemplate cloning a PVC at the imported PVC")
		Expect(vm.Spec.DataVolumeTemplates).To(ContainElement(clonePVCSource(rootPVC)))
		By("pointing the PVC volume at the imported PVC")
		Expect(vm.Spec.Template.Spec.Volumes).To(ContainElement(And(
			HaveField("Name", artifacttest.DataDisk),
			HaveField("PersistentVolumeClaim.ClaimName", dataPVC),
		)))
		By("keeping the DataVolumeTemplate importing from a DataSource")
		Expect(vm.Spec.DataVolumeTemplates).To(ContainElement(HaveField("Spec.SourceRef.Kind", cdiv1beta1.DataVolumeDataSource)))

		Expect(diskChecksum(testNamespace, rootPVC)).To(Equal(expectedChecksum(artifacttest.RootDisk)))
		Expect(diskChecksum(testNamespace, dataPVC)).To(Equal(expectedChecksum(artifacttest.DataDisk)))
	})

	DescribeTable("should create a working VirtualMachine from an imported VirtualMachineTemplate", func(inAnotherNamespace bool) {
		vmImport := waitForImportReady(createImport(fixtureURL(artifacttest.VMTemplateDVT)).Name)
		vmName := "fromtemplate-" + rand.String(5)
		templates := templateClient.TemplateV1beta1().VirtualMachineTemplates(testNamespace)

		namespace := testNamespace
		if !inAnotherNamespace {
			By("creating the VirtualMachine through the template's create subresource")
			processTemplate(templates.CreateVirtualMachine, vmImport.Name, vmName)
		} else {
			namespace = createNamespace()

			By("allowing VirtualMachines in the other namespace to clone the imported PVCs")
			allowCloneFrom(testNamespace, namespace)

			By("processing the template and creating the VirtualMachine in another namespace")
			vm := processTemplate(templates.Process, vmImport.Name, vmName).VirtualMachine
			vm.Namespace = namespace
			_, err := virtClient.KubevirtV1().VirtualMachines(namespace).Create(context.Background(), vm, metav1.CreateOptions{})
			Expect(err).ToNot(HaveOccurred())
		}

		By("cloning the imported disk into the VirtualMachine's DataVolume")
		dvName := vmName + "-" + artifacttest.RootDisk
		Eventually(func(g Gomega) {
			dv, err := cdiClient.CdiV1beta1().DataVolumes(namespace).Get(context.Background(), dvName, metav1.GetOptions{})
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(dv.Status.Phase).ToNot(Equal(cdiv1beta1.Failed), "DataVolume failed: %v", dv.Status.Conditions)
			g.Expect(dv.Status.Phase).To(Equal(cdiv1beta1.Succeeded))
		}, importTimeout, pollInterval).Should(Succeed())
		Expect(diskChecksum(namespace, dvName)).To(Equal(expectedChecksum(artifacttest.RootDisk)))
	},
		Entry("in the import namespace", false),
		Entry("in another namespace", true),
	)

	It("should keep the template and its disks when the import is deleted", func() {
		vmImport := waitForImportReady(createImport(fixtureURL(artifacttest.VMTemplateDVT)).Name)
		deleteImport(vmImport.Name)

		getTemplate(vmImport.Name)
		expectPVCKept(dataVolumeNameOf(vmImport, artifacttest.RootDisk))
	})

	It("should not overwrite an existing VirtualMachineTemplate", func() {
		name := "existing-" + rand.String(5)
		existing := &templatev1beta1.VirtualMachineTemplate{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec: templatev1beta1.VirtualMachineTemplateSpec{
				VirtualMachine: &runtime.RawExtension{Raw: []byte(`{"apiVersion":"kubevirt.io/v1","kind":"VirtualMachine","spec":{}}`)},
			},
		}
		_, err := templateClient.TemplateV1beta1().VirtualMachineTemplates(testNamespace).
			Create(context.Background(), existing, metav1.CreateOptions{})
		Expect(err).ToNot(HaveOccurred())

		vmImport := createImport(fixtureURL(artifacttest.VMTemplateDVT), withTargetName(name))
		waitForImportFailed(vmImport.Name, ContainSubstring("VirtualMachineTemplate "+testNamespace+"/"+name+" already exists"))

		Expect(getTemplate(name).Labels).ToNot(HaveKey(v1alpha1.LabelImportUID))
	})
})

// allowCloneFrom lets the default ServiceAccount of targetNamespace clone PVCs from
// sourceNamespace, which KubeVirt requires for DataVolumeTemplates cloning across namespaces.
func allowCloneFrom(sourceNamespace, targetNamespace string) {
	GinkgoHelper()
	const generateName = "datavolume-cloner-"
	role, err := kubeClient.RbacV1().Roles(sourceNamespace).Create(context.Background(), &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{GenerateName: generateName},
		Rules: []rbacv1.PolicyRule{{
			APIGroups: []string{cdiv1beta1.SchemeGroupVersion.Group},
			Resources: []string{"datavolumes/source"},
			Verbs:     []string{"create"},
		}},
	}, metav1.CreateOptions{})
	Expect(err).ToNot(HaveOccurred())
	_, err = kubeClient.RbacV1().RoleBindings(sourceNamespace).Create(context.Background(), &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{GenerateName: generateName},
		Subjects:   []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Name: "default", Namespace: targetNamespace}},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: role.Name},
	}, metav1.CreateOptions{})
	Expect(err).ToNot(HaveOccurred())
}

var templateGVK = templatev1beta1.GroupVersion.WithKind("VirtualMachineTemplate")

// clonePVCSource matches a DataVolumeTemplate cloning pvcName from the test namespace.
func clonePVCSource(pvcName string) gomegatypes.GomegaMatcher {
	return HaveField("Spec.Source.PVC", Equal(&cdiv1beta1.DataVolumeSourcePVC{Namespace: testNamespace, Name: pvcName}))
}

func getTemplate(name string) *templatev1beta1.VirtualMachineTemplate {
	GinkgoHelper()
	tpl, err := templateClient.TemplateV1beta1().VirtualMachineTemplates(testNamespace).
		Get(context.Background(), name, metav1.GetOptions{})
	Expect(err).ToNot(HaveOccurred())
	return tpl
}

func templateVM(tpl *templatev1beta1.VirtualMachineTemplate) *virtv1.VirtualMachine {
	GinkgoHelper()
	vm := &virtv1.VirtualMachine{}
	Expect(json.Unmarshal(tpl.Spec.VirtualMachine.Raw, vm)).To(Succeed())
	return vm
}

// processTemplate calls process, the process or create subresource of an imported template, for a
// VirtualMachine named vmName, retrying until virt-template accepts the template.
func processTemplate(
	process func(context.Context, string, subresourcesv1beta1.ProcessOptions) (*subresourcesv1beta1.ProcessedVirtualMachineTemplate, error),
	templateName, vmName string,
) *subresourcesv1beta1.ProcessedVirtualMachineTemplate {
	GinkgoHelper()
	var processed *subresourcesv1beta1.ProcessedVirtualMachineTemplate
	Eventually(func(g Gomega) {
		var err error
		processed, err = process(context.Background(), templateName,
			subresourcesv1beta1.ProcessOptions{Parameters: map[string]string{"NAME": vmName}})
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(processed.VirtualMachine).ToNot(BeNil())
	}, importTimeout, pollInterval).Should(Succeed())

	return processed
}
