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
	"flag"
	"os"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	ginkgo_reporters "github.com/onsi/ginkgo/v2/reporters"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/layout"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/types"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	cdiclient "kubevirt.io/client-go/containerizeddataimporter"
	kubevirtclient "kubevirt.io/client-go/kubevirt"
	qe_reporters "kubevirt.io/qe-tools/pkg/ginkgo-reporters"

	templateclient "kubevirt.io/virt-template-client-go/virttemplate"

	importclient "kubevirt.io/virt-import-client-go/virtimport"

	"kubevirt.io/virt-import/internal/artifact/artifacttest"
)

const (
	// NamespaceVirtImport is where virt-import is deployed.
	NamespaceVirtImport = "kubevirt"

	testRepositoryPrefix = "virt-import-tests/"
	testTag              = "latest"
)

var (
	// testNamespace is created for every spec and deleted after it, with everything in it.
	testNamespace string

	restConfig     *rest.Config
	kubeClient     kubernetes.Interface
	virtClient     kubevirtclient.Interface
	cdiClient      cdiclient.Interface
	importClient   importclient.Interface
	templateClient templateclient.Interface

	testRegistry = flag.String("registry", "registry:5000",
		"registry the test artifact fixtures are pushed to, as seen from the cluster")
	pushRegistry = flag.String("push-registry", "", "the same registry as seen from the test host (required)")
	testImage    = flag.String("test-image", "registry:5000/virt-import-functest:latest",
		"image of the helper Pods (tests/image/Dockerfile)")

	afterSuiteReporters []Reporter
)

var _ = BeforeSuite(func() {
	kubeconfigPath := os.Getenv("KUBECONFIG")
	if _, err := os.Stat(kubeconfigPath); os.IsNotExist(err) {
		Skip("KUBECONFIG not found, skipping tests")
	}
	Expect(*pushRegistry).ToNot(BeEmpty(), "-push-registry must be set to the test registry as seen from the test host")

	var err error
	restConfig, err = clientcmd.BuildConfigFromFlags("", kubeconfigPath)
	Expect(err).NotTo(HaveOccurred())

	kubeClient, err = kubernetes.NewForConfig(restConfig)
	Expect(err).NotTo(HaveOccurred())

	virtClient, err = kubevirtclient.NewForConfig(restConfig)
	Expect(err).NotTo(HaveOccurred())

	cdiClient, err = cdiclient.NewForConfig(restConfig)
	Expect(err).NotTo(HaveOccurred())

	importClient, err = importclient.NewForConfig(restConfig)
	Expect(err).NotTo(HaveOccurred())

	templateClient, err = templateclient.NewForConfig(restConfig)
	Expect(err).NotTo(HaveOccurred())

	for fixtureName, fixture := range artifacttest.Fixtures() {
		pushFixture(fixtureName, fixture)
	}
})

var _ = BeforeEach(func() {
	testNamespace = createNamespace()
})

var _ = ReportAfterSuite("TestFunctional", func(report Report) {
	for _, reporter := range afterSuiteReporters {
		//nolint:staticcheck
		ginkgo_reporters.ReportViaDeprecatedReporter(reporter, report)
	}
})

func TestFunctional(t *testing.T) {
	if qe_reporters.JunitOutput != "" {
		afterSuiteReporters = append(afterSuiteReporters, ginkgo_reporters.NewJUnitReporter(qe_reporters.JunitOutput))
	}
	if qe_reporters.Polarion.Run {
		afterSuiteReporters = append(afterSuiteReporters, &qe_reporters.Polarion)
	}

	RegisterFailHandler(Fail)
	RunSpecs(t, "Functional test suite")
}

// pushFixture pushes a fixture as <testRepositoryPrefix><fixtureName>:<testTag> to the test
// registry, as the test host sees it.
func pushFixture(fixtureName string, fixture *artifacttest.Artifact) {
	GinkgoHelper()
	dir := GinkgoT().TempDir()
	Expect(fixture.WriteLayout(dir)).To(Succeed(), "failed to write fixture %s", fixtureName)
	path, err := layout.FromPath(dir)
	Expect(err).ToNot(HaveOccurred())
	index, err := path.ImageIndex()
	Expect(err).ToNot(HaveOccurred())
	indexManifest, err := index.IndexManifest()
	Expect(err).ToNot(HaveOccurred())
	Expect(indexManifest.Manifests).To(HaveLen(1))
	top := indexManifest.Manifests[0]

	ref, err := name.NewTag(*pushRegistry+"/"+testRepositoryPrefix+fixtureName+":"+testTag, name.Insecure)
	Expect(err).ToNot(HaveOccurred())
	opt := remote.WithContext(context.Background())
	if top.MediaType == types.OCIImageIndex {
		child, err := index.ImageIndex(top.Digest)
		Expect(err).ToNot(HaveOccurred())
		Expect(remote.WriteIndex(ref, child, opt)).To(Succeed(), "failed to push fixture %s", fixtureName)
	} else {
		image, err := index.Image(top.Digest)
		Expect(err).ToNot(HaveOccurred())
		Expect(remote.Write(ref, image, opt)).To(Succeed(), "failed to push fixture %s", fixtureName)
	}
}

// createNamespace creates a namespace that is deleted, with everything in it, after the spec.
func createNamespace() string {
	GinkgoHelper()
	namespace, err := kubeClient.CoreV1().Namespaces().Create(context.Background(), &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "vm-import-test-"},
	}, metav1.CreateOptions{})
	Expect(err).NotTo(HaveOccurred())
	DeferCleanup(func() {
		Expect(kubeClient.CoreV1().Namespaces().Delete(context.Background(), namespace.Name, metav1.DeleteOptions{})).
			To(Succeed())
	})
	return namespace.Name
}
