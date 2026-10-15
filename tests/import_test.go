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
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	gomegatypes "github.com/onsi/gomega/types"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/utils/ptr"

	virtv1 "kubevirt.io/api/core/v1"

	"kubevirt.io/virt-import-api/core/v1alpha1"

	"kubevirt.io/virt-import/internal/artifact/artifacttest"
)

const (
	// qemuUID is the user and group of QEMU in KubeVirt and of the disk images CDI imports.
	qemuUID int64 = 107

	importTimeout = 5 * time.Minute
	// fetchAttempts is how often the metadata-fetch Job runs the fetcher: its backoff limit of 3
	// plus the first attempt.
	fetchAttempts = 4
	pollInterval  = time.Second
)

var _ = Describe("VirtualMachineImport", func() {
	DescribeTable("should import a VirtualMachine", func(fixture string, opt importOpt, disks ...string) {
		vmImport := waitForImportReady(createImport(fixtureURL(fixture), opt).Name)

		Expect(vmImport.Status.ArtifactInfo).ToNot(BeNil())
		Expect(vmImport.Status.ArtifactInfo.Digest).To(HavePrefix("sha256:"))
		Expect(vmImport.Status.TargetRef).ToNot(BeNil())
		Expect(vmImport.Status.TargetRef.Kind).To(Equal(virtv1.VirtualMachineGroupVersionKind.Kind))

		vm, err := virtClient.KubevirtV1().VirtualMachines(testNamespace).Get(context.Background(), vmImport.Name, metav1.GetOptions{})
		Expect(err).ToNot(HaveOccurred())
		Expect(vm.Labels).To(HaveKeyWithValue(v1alpha1.LabelImportUID, string(vmImport.UID)))

		claims := claimNames(vm)
		Expect(vmImport.Status.DiskImports).To(HaveLen(len(disks)))
		Expect(vmImport.Status.DiskImports).To(HaveEach(HaveField("Progress", "100.0%")))
		for _, disk := range disks {
			pvcName := dataVolumeNameOf(vmImport, disk)
			Expect(claims).To(HaveKeyWithValue(disk, pvcName), "volume %s should use the imported PVC", disk)
			Expect(diskChecksum(testNamespace, pvcName)).To(Equal(expectedChecksum(disk)), "disk %s should hold the artifact's data", disk)
			waitForDataVolumeHandover(pvcName, vm)
		}
		expectNoLeftovers(vmImport)
	},
		Entry("with a single disk", artifacttest.VMSingle, nil, artifacttest.RootDisk),
		Entry("with several disks", artifacttest.VMMultiDisk, nil, artifacttest.RootDisk, artifacttest.DataDisk),
		Entry("for the selected architecture", artifacttest.VMMultiArch, withArchitecture(artifacttest.ArchARM64),
			artifacttest.RootDisk),
		Entry("from a plain manifest", artifacttest.VMPlainManifest, nil, artifacttest.RootDisk),
	)

	It("should create the target under the target name", func() {
		name := "named-" + rand.String(5)
		vmImport := waitForImportReady(createImport(fixtureURL(artifacttest.VMSingle), withTargetName(name)).Name)
		Expect(vmImport.Status.TargetRef.Name).To(Equal(name))

		vm, err := virtClient.KubevirtV1().VirtualMachines(testNamespace).Get(context.Background(), name, metav1.GetOptions{})
		Expect(err).ToNot(HaveOccurred())
		Expect(vm.Labels).To(HaveKeyWithValue(v1alpha1.LabelImportUID, string(vmImport.UID)))
		_, err = virtClient.KubevirtV1().VirtualMachines(testNamespace).Get(context.Background(), vmImport.Name, metav1.GetOptions{})
		Expect(err).To(MatchError(k8serrors.IsNotFound, "k8serrors.IsNotFound"))
	})

	It("should keep the target and its disks when the import is deleted", func() {
		vmImport := waitForImportReady(createImport(fixtureURL(artifacttest.VMSingle)).Name)
		deleteImport(vmImport.Name)

		_, err := virtClient.KubevirtV1().VirtualMachines(testNamespace).Get(context.Background(), vmImport.Name, metav1.GetOptions{})
		Expect(err).ToNot(HaveOccurred())
		expectPVCKept(dataVolumeNameOf(vmImport, artifacttest.RootDisk))
	})

	It("should delete everything when deleted before completion", func() {
		vmImport := createImport(fixtureURL(artifacttest.VMSingle), withStorageClass("does-not-exist"))
		Eventually(func(g Gomega) {
			vmImport = getImport(g, vmImport.Name)
			g.Expect(vmImport.Status.DiskImports).To(HaveLen(1))
		}, importTimeout, pollInterval).Should(Succeed())
		dvName := dataVolumeNameOf(vmImport, artifacttest.RootDisk)

		deleteImport(vmImport.Name)
		Eventually(func() error {
			_, err := cdiClient.CdiV1beta1().DataVolumes(testNamespace).Get(context.Background(), dvName, metav1.GetOptions{})
			return err
		}, importTimeout, pollInterval).Should(MatchError(k8serrors.IsNotFound, "k8serrors.IsNotFound"))
		expectNoLeftovers(vmImport)
	})

	It("should resume after a controller restart", Serial, func() {
		// The disk import waits for this StorageClass, which is only created after the restart.
		storageClassName := "vmimport-restart-" + rand.String(5)
		vmImport := createImport(fixtureURL(artifacttest.VMSingle), withStorageClass(storageClassName))
		Eventually(func(g Gomega) {
			vmImport = getImport(g, vmImport.Name)
			g.Expect(vmImport.Status.DiskImports).To(HaveLen(1))
		}, importTimeout, pollInterval).Should(Succeed())
		dv, err := cdiClient.CdiV1beta1().DataVolumes(testNamespace).
			Get(context.Background(), vmImport.Status.DiskImports[0].DataVolumeName, metav1.GetOptions{})
		Expect(err).ToNot(HaveOccurred())

		restartController()

		Consistently(func(g Gomega) {
			g.Expect(getImport(g, vmImport.Name)).
				To(haveCondition(v1alpha1.ConditionProgressing, metav1.ConditionTrue, v1alpha1.ReasonImportingDisks))
			current, getErr := cdiClient.CdiV1beta1().DataVolumes(testNamespace).Get(context.Background(), dv.Name, metav1.GetOptions{})
			g.Expect(getErr).ToNot(HaveOccurred())
			g.Expect(current.UID).To(Equal(dv.UID))
		}, 20*time.Second, pollInterval).Should(Succeed())
		// The manifest ConfigMap stays until the disks are imported.
		expectNoFetchJob(vmImport)

		By("completing the import once the StorageClass exists")
		createDefaultStorageClassCopy(storageClassName)
		vmImport = waitForImportReady(vmImport.Name)
		Expect(dataVolumeNameOf(vmImport, artifacttest.RootDisk)).To(Equal(dv.Name))
		pvc, err := kubeClient.CoreV1().PersistentVolumeClaims(testNamespace).Get(context.Background(), dv.Name, metav1.GetOptions{})
		Expect(err).ToNot(HaveOccurred())
		Expect(pvc.Spec.StorageClassName).To(HaveValue(Equal(storageClassName)))
		Expect(diskChecksum(testNamespace, dv.Name)).To(Equal(expectedChecksum(artifacttest.RootDisk)))
	})

	DescribeTable("should fail the import of an invalid artifact", func(fixture string, opt importOpt, message string) {
		vmImport := createImport(fixtureURL(fixture), opt)
		waitForImportFailed(vmImport.Name, ContainSubstring(message))
		By("failing on the first attempt, as the artifact is invalid")
		Expect(getFetchJob(vmImport).Status.Failed).To(BeEquivalentTo(1))
	},
		Entry("with an unsupported artifactType", artifacttest.BadArtifactType, nil, "unsupported artifactType"),
		Entry("with a disk without size", artifacttest.MissingSize, nil, "no io.kubevirt.disk.size annotation"),
		Entry("with a duplicate disk", artifacttest.DuplicateDisk, nil, "named by more than one layer"),
		Entry("with a disk naming no volume", artifacttest.OrphanLayer, nil, `disk layer "extradisk" does not name a PVC volume`),
		Entry("with a volume without disk", artifacttest.UnlayeredPVC, nil, `PVC volume "datadisk" is not named by any disk layer`),
		Entry("with an unknown layer", artifacttest.UnknownLayerMediaType, nil, "unsupported media type"),
		Entry("with several architectures and none selected", artifacttest.VMMultiArch, nil, "several architectures (amd64, arm64)"),
		Entry("with an architecture it lacks", artifacttest.VMPlainManifest, withArchitecture(artifacttest.ArchARM64),
			`artifact is for architecture "amd64", "arm64" was requested`),
	)

	It("should fail the import of a missing artifact after retrying", func() {
		vmImport := createImport(fixtureURL("does-not-exist"))
		waitForImportFailed(vmImport.Name, ContainSubstring("manifest unknown"))
		Expect(getFetchJob(vmImport).Status.Failed).To(BeEquivalentTo(fetchAttempts))
	})

	It("should report a missing registry Secret", func() {
		vmImport := createImport(fixtureURL(artifacttest.VMSingle), withSecret("does-not-exist"))
		Eventually(func(g Gomega) {
			g.Expect(getImport(g, vmImport.Name)).To(haveCondition(v1alpha1.ConditionReady, metav1.ConditionFalse,
				v1alpha1.ReasonFetchingMetadata, ContainSubstring("cannot start: CreateContainerConfigError")))
		}, importTimeout, pollInterval).Should(Succeed())
	})

	It("should not overwrite an existing target", func() {
		name := "existing-" + rand.String(5)
		vm := &virtv1.VirtualMachine{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec: virtv1.VirtualMachineSpec{
				RunStrategy: ptr.To(virtv1.RunStrategyHalted),
				Template:    &virtv1.VirtualMachineInstanceTemplateSpec{},
			},
		}
		_, err := virtClient.KubevirtV1().VirtualMachines(testNamespace).Create(context.Background(), vm, metav1.CreateOptions{})
		Expect(err).ToNot(HaveOccurred())

		vmImport := createImport(fixtureURL(artifacttest.VMSingle), withTargetName(name))
		waitForImportFailed(vmImport.Name, ContainSubstring("VirtualMachine "+testNamespace+"/"+name+" already exists"))
	})
})

type importOpt func(*v1alpha1.VirtualMachineImport)

func withArchitecture(arch string) importOpt {
	return func(vmImport *v1alpha1.VirtualMachineImport) {
		vmImport.Spec.Source.Registry.Platform = &v1alpha1.PlatformOptions{Architecture: arch}
	}
}

func withStorageClass(name string) importOpt {
	return func(vmImport *v1alpha1.VirtualMachineImport) {
		vmImport.Spec.StorageClassName = name
	}
}

func withSecret(name string) importOpt {
	return func(vmImport *v1alpha1.VirtualMachineImport) {
		vmImport.Spec.Source.Registry.SecretRef = name
	}
}

func withTargetName(name string) importOpt {
	return func(vmImport *v1alpha1.VirtualMachineImport) {
		vmImport.Spec.TargetName = name
	}
}

func fixtureURL(name string) string {
	return "docker://" + *testRegistry + "/" + testRepositoryPrefix + name + ":" + testTag
}

func newImport(url string, opts ...importOpt) *v1alpha1.VirtualMachineImport {
	vmImport := &v1alpha1.VirtualMachineImport{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "vmimport-"},
		Spec: v1alpha1.VirtualMachineImportSpec{
			Source: v1alpha1.VirtualMachineImportSource{
				Registry: &v1alpha1.VirtualMachineImportRegistrySource{URL: url},
			},
		},
	}
	for _, opt := range opts {
		if opt != nil {
			opt(vmImport)
		}
	}
	return vmImport
}

func createImport(url string, opts ...importOpt) *v1alpha1.VirtualMachineImport {
	GinkgoHelper()
	vmImport, err := importClient.ImportV1alpha1().VirtualMachineImports(testNamespace).
		Create(context.Background(), newImport(url, opts...), metav1.CreateOptions{})
	Expect(err).ToNot(HaveOccurred())
	return vmImport
}

func getImport(g Gomega, name string) *v1alpha1.VirtualMachineImport {
	vmImport, err := importClient.ImportV1alpha1().VirtualMachineImports(testNamespace).Get(context.Background(), name, metav1.GetOptions{})
	g.Expect(err).ToNot(HaveOccurred())
	return vmImport
}

func waitForImportReady(name string) *v1alpha1.VirtualMachineImport {
	GinkgoHelper()
	var vmImport *v1alpha1.VirtualMachineImport
	Eventually(func(g Gomega) {
		vmImport = getImport(g, name)
		progressing := meta.FindStatusCondition(vmImport.Status.Conditions, v1alpha1.ConditionProgressing)
		if progressing != nil && progressing.Reason == v1alpha1.ReasonImportFailed {
			StopTrying(fmt.Sprintf("import failed: %v", vmImport.Status.Conditions)).Now()
		}
		g.Expect(vmImport).To(haveCondition(v1alpha1.ConditionReady, metav1.ConditionTrue, v1alpha1.ReasonImportComplete))
	}, importTimeout, pollInterval).Should(Succeed())
	return vmImport
}

func waitForImportFailed(name string, messageMatcher gomegatypes.GomegaMatcher) {
	GinkgoHelper()
	Eventually(func(g Gomega) {
		g.Expect(getImport(g, name)).To(And(
			haveCondition(v1alpha1.ConditionProgressing, metav1.ConditionFalse, v1alpha1.ReasonImportFailed),
			haveCondition(v1alpha1.ConditionReady, metav1.ConditionFalse, v1alpha1.ReasonImportFailed, messageMatcher),
		))
	}, importTimeout, pollInterval).Should(Succeed())
}

func deleteImport(name string) {
	GinkgoHelper()
	Expect(importClient.ImportV1alpha1().VirtualMachineImports(testNamespace).
		Delete(context.Background(), name, metav1.DeleteOptions{})).To(Succeed())
	Eventually(func() error {
		_, err := importClient.ImportV1alpha1().VirtualMachineImports(testNamespace).Get(context.Background(), name, metav1.GetOptions{})
		return err
	}, importTimeout, pollInterval).Should(MatchError(k8serrors.IsNotFound, "k8serrors.IsNotFound"))
}

func expectPVCKept(pvcName string) {
	GinkgoHelper()
	Consistently(func() error {
		_, err := kubeClient.CoreV1().PersistentVolumeClaims(testNamespace).Get(context.Background(), pvcName, metav1.GetOptions{})
		return err
	}, 10*time.Second, pollInterval).Should(Succeed())
}

// waitForDataVolumeHandover waits until the DataVolume of an imported PVC is controlled by the
// target vm instead of the import.
func waitForDataVolumeHandover(dvName string, vm *virtv1.VirtualMachine) {
	GinkgoHelper()
	Eventually(func(g Gomega) {
		dv, err := cdiClient.CdiV1beta1().DataVolumes(testNamespace).Get(context.Background(), dvName, metav1.GetOptions{})
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(dv.OwnerReferences).To(ConsistOf(*metav1.NewControllerRef(vm, virtv1.VirtualMachineGroupVersionKind)))
	}, importTimeout, pollInterval).Should(Succeed())
}

// getFetchJob returns the metadata-fetch Job of an import. A failed import keeps it.
func getFetchJob(vmImport *v1alpha1.VirtualMachineImport) *batchv1.Job {
	GinkgoHelper()
	jobs, err := kubeClient.BatchV1().Jobs(testNamespace).List(context.Background(), importListOptions(vmImport))
	Expect(err).ToNot(HaveOccurred())
	Expect(jobs.Items).To(HaveLen(1))
	return &jobs.Items[0]
}

func expectNoFetchJob(vmImport *v1alpha1.VirtualMachineImport) {
	GinkgoHelper()
	Eventually(func(g Gomega) {
		jobs, err := kubeClient.BatchV1().Jobs(testNamespace).List(context.Background(), importListOptions(vmImport))
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(jobs.Items).To(BeEmpty())
	}, importTimeout, pollInterval).Should(Succeed())
}

// expectNoLeftovers waits until the fetch Job and the manifest ConfigMap of an import are gone.
func expectNoLeftovers(vmImport *v1alpha1.VirtualMachineImport) {
	GinkgoHelper()
	expectNoFetchJob(vmImport)
	Eventually(func(g Gomega) {
		configMaps, err := kubeClient.CoreV1().ConfigMaps(testNamespace).List(context.Background(), importListOptions(vmImport))
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(configMaps.Items).To(BeEmpty())
	}, importTimeout, pollInterval).Should(Succeed())
}

func importListOptions(vmImport *v1alpha1.VirtualMachineImport) metav1.ListOptions {
	return metav1.ListOptions{LabelSelector: v1alpha1.LabelImportUID + "=" + string(vmImport.UID)}
}

// createDefaultStorageClassCopy creates a StorageClass named name with the provisioner and
// settings of the cluster's default StorageClass.
func createDefaultStorageClassCopy(name string) {
	GinkgoHelper()
	classes, err := kubeClient.StorageV1().StorageClasses().List(context.Background(), metav1.ListOptions{})
	Expect(err).ToNot(HaveOccurred())
	var defaultClass *storagev1.StorageClass
	for i := range classes.Items {
		if classes.Items[i].Annotations["storageclass.kubernetes.io/is-default-class"] == "true" {
			defaultClass = &classes.Items[i]
		}
	}
	Expect(defaultClass).ToNot(BeNil(), "the cluster has no default StorageClass")

	_, err = kubeClient.StorageV1().StorageClasses().Create(context.Background(), &storagev1.StorageClass{
		ObjectMeta:           metav1.ObjectMeta{Name: name},
		Provisioner:          defaultClass.Provisioner,
		Parameters:           defaultClass.Parameters,
		ReclaimPolicy:        defaultClass.ReclaimPolicy,
		MountOptions:         defaultClass.MountOptions,
		AllowVolumeExpansion: defaultClass.AllowVolumeExpansion,
		VolumeBindingMode:    defaultClass.VolumeBindingMode,
	}, metav1.CreateOptions{})
	Expect(err).ToNot(HaveOccurred())
	DeferCleanup(func() {
		Expect(kubeClient.StorageV1().StorageClasses().Delete(context.Background(), name, metav1.DeleteOptions{})).
			To(Or(Succeed(), MatchError(k8serrors.IsNotFound, "k8serrors.IsNotFound")))
	})
}

func haveCondition(
	conditionType string, status metav1.ConditionStatus, reason string, messageMatchers ...gomegatypes.GomegaMatcher,
) gomegatypes.GomegaMatcher {
	matchers := []gomegatypes.GomegaMatcher{
		HaveField("Type", conditionType), HaveField("Status", status), HaveField("Reason", reason),
	}
	for _, matcher := range messageMatchers {
		matchers = append(matchers, HaveField("Message", matcher))
	}
	return HaveField("Status.Conditions", ContainElement(And(matchers...)))
}

// dataVolumeNameOf returns the name of the DataVolume, and PVC, a disk was imported into.
func dataVolumeNameOf(vmImport *v1alpha1.VirtualMachineImport, disk string) string {
	GinkgoHelper()
	for _, diskImport := range vmImport.Status.DiskImports {
		if diskImport.Name == disk {
			return diskImport.DataVolumeName
		}
	}
	Fail(fmt.Sprintf("disk %s not found in %v", disk, vmImport.Status.DiskImports))
	return ""
}

func shellCommand(script string) []string {
	return []string{"sh", "-c", script}
}

func claimNames(vm *virtv1.VirtualMachine) map[string]string {
	claims := map[string]string{}
	for _, vol := range vm.Spec.Template.Spec.Volumes {
		if vol.PersistentVolumeClaim != nil {
			claims[vol.Name] = vol.PersistentVolumeClaim.ClaimName
		}
	}
	return claims
}

func expectedChecksum(disk string) string {
	sum := sha256.Sum256(artifacttest.DiskContent(disk, artifacttest.DiskSize))
	return hex.EncodeToString(sum[:])
}

// diskChecksum returns the SHA-256 of the first artifacttest.DiskSize bytes of the disk in a PVC.
func diskChecksum(namespace, pvcName string) string {
	GinkgoHelper()
	pvc, err := kubeClient.CoreV1().PersistentVolumeClaims(namespace).Get(context.Background(), pvcName, metav1.GetOptions{})
	Expect(err).ToNot(HaveOccurred())

	const (
		volumeName = "disk"
		devicePath = "/dev/disk"
		mountPath  = "/disk"
	)
	path := mountPath + "/disk.img"
	container := corev1.Container{
		Name:  "checksum",
		Image: *testImage,
	}
	if ptr.Deref(pvc.Spec.VolumeMode, corev1.PersistentVolumeFilesystem) == corev1.PersistentVolumeBlock {
		path = devicePath
		container.VolumeDevices = []corev1.VolumeDevice{{Name: volumeName, DevicePath: devicePath}}
	} else {
		container.VolumeMounts = []corev1.VolumeMount{{Name: volumeName, MountPath: mountPath, ReadOnly: true}}
	}
	container.Command = shellCommand(fmt.Sprintf("set -o pipefail; head -c %d %s | sha256sum", artifacttest.DiskSize, path))

	logs := runPod(namespace, &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "checksum-"},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			Containers:    []corev1.Container{container},
			Volumes: []corev1.Volume{{
				Name: volumeName,
				VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
					ClaimName: pvcName, ReadOnly: true,
				}},
			}},
		},
	})
	fields := strings.Fields(logs)
	Expect(fields).ToNot(BeEmpty(), "checksum Pod printed nothing")
	return fields[0]
}

// runPod runs a Pod to completion under the restricted Pod Security Standard and returns its logs.
func runPod(namespace string, pod *corev1.Pod) string {
	GinkgoHelper()
	restrictPod(pod)
	pod, err := kubeClient.CoreV1().Pods(namespace).Create(context.Background(), pod, metav1.CreateOptions{})
	Expect(err).ToNot(HaveOccurred())

	Eventually(func(g Gomega) {
		current, err := kubeClient.CoreV1().Pods(namespace).Get(context.Background(), pod.Name, metav1.GetOptions{})
		g.Expect(err).ToNot(HaveOccurred())
		if current.Status.Phase == corev1.PodFailed {
			StopTrying(fmt.Sprintf("pod %s failed: %s", pod.Name, podLogs(namespace, pod.Name))).Now()
		}
		g.Expect(current.Status.Phase).To(Equal(corev1.PodSucceeded))
	}, importTimeout, pollInterval).Should(Succeed())

	return podLogs(namespace, pod.Name)
}

// restrictPod applies the restricted Pod Security Standard like KubeVirt's libpod.RenderPod. The
// Pod runs as qemu, the owner of the disk images CDI imports.
func restrictPod(pod *corev1.Pod) {
	pod.Spec.SecurityContext = &corev1.PodSecurityContext{
		RunAsNonRoot:   ptr.To(true),
		RunAsUser:      ptr.To(qemuUID),
		RunAsGroup:     ptr.To(qemuUID),
		FSGroup:        ptr.To(qemuUID),
		SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
	}
	for i := range pod.Spec.Containers {
		pod.Spec.Containers[i].SecurityContext = &corev1.SecurityContext{
			AllowPrivilegeEscalation: ptr.To(false),
			Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		}
	}
}

func podLogs(namespace, name string) string {
	logs, err := kubeClient.CoreV1().Pods(namespace).GetLogs(name, &corev1.PodLogOptions{}).DoRaw(context.Background())
	if err != nil {
		return err.Error()
	}
	return string(logs)
}

func restartController() {
	GinkgoHelper()
	const selector = "control-plane=controller-manager,app.kubernetes.io/name=virt-import"
	pods, err := kubeClient.CoreV1().Pods(NamespaceVirtImport).List(context.Background(), metav1.ListOptions{LabelSelector: selector})
	Expect(err).ToNot(HaveOccurred())
	Expect(pods.Items).ToNot(BeEmpty())
	for i := range pods.Items {
		Expect(kubeClient.CoreV1().Pods(NamespaceVirtImport).Delete(context.Background(), pods.Items[i].Name, metav1.DeleteOptions{})).
			To(Succeed())
	}

	Eventually(func(g Gomega) {
		current, err := kubeClient.CoreV1().Pods(NamespaceVirtImport).List(context.Background(), metav1.ListOptions{LabelSelector: selector})
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(current.Items).To(ContainElement(And(
			Not(HaveField("UID", BeElementOf(podUIDs(pods.Items)))),
			HaveField("Status.Conditions", ContainElement(And(
				HaveField("Type", corev1.PodReady), HaveField("Status", corev1.ConditionTrue),
			))),
		)))
	}, importTimeout, pollInterval).Should(Succeed())
}

func podUIDs(pods []corev1.Pod) []types.UID {
	uids := make([]types.UID, 0, len(pods))
	for i := range pods {
		uids = append(uids, pods[i].UID)
	}
	return uids
}
