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
package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	csvv1 "github.com/operator-framework/api/pkg/operators/v1alpha1"
	corev1 "k8s.io/api/core/v1"
)

const testManifests = `
apiVersion: v1
kind: ServiceAccount
metadata:
  name: virt-import-controller-manager
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: virt-import-manager-role
rules:
- apiGroups: ["import.kubevirt.io"]
  resources: ["virtualmachineimports"]
  verbs: ["get", "list", "watch"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: virt-import-metrics-auth-role
rules:
- apiGroups: ["authentication.k8s.io"]
  resources: ["tokenreviews"]
  verbs: ["create"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: virt-import-unbound-role
rules:
- apiGroups: [""]
  resources: ["pods"]
  verbs: ["get"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: virt-import-manager-rolebinding
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: virt-import-manager-role
subjects:
- kind: ServiceAccount
  name: virt-import-controller-manager
  namespace: kubevirt
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: virt-import-metrics-auth-rolebinding
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: virt-import-metrics-auth-role
subjects:
- kind: ServiceAccount
  name: virt-import-controller-manager
  namespace: kubevirt
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: virt-import-foreign-rolebinding
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: virt-import-unbound-role
subjects:
- kind: ServiceAccount
  name: someone-else
  namespace: other
---
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: virt-import-leader-election-role
rules:
- apiGroups: ["coordination.k8s.io"]
  resources: ["leases"]
  verbs: ["get", "create"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: virt-import-leader-election-rolebinding
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: virt-import-leader-election-role
subjects:
- kind: ServiceAccount
  name: virt-import-controller-manager
  namespace: kubevirt
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: virt-import-controller
  labels:
    control-plane: controller-manager
spec:
  template:
    spec:
      containers:
      - name: manager
        image: controller:latest
        args: ["--leader-elect"]
        env:
        - name: FETCHER_IMAGE
          value: quay.io/kubevirt/virt-import-fetcher:latest
---
apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: virtualmachineimports.import.kubevirt.io
---
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: virt-import-deny-all
---
apiVersion: v1
kind: Service
metadata:
  name: virt-import-controller-manager-metrics-service
`

const (
	operatorImage        = "quay.io/kubevirt/virt-import-controller:v1.2.3"
	renderedFetcherImage = "quay.io/kubevirt/virt-import-fetcher:latest"
	fetcherImage         = "quay.io/kubevirt/virt-import-fetcher:v1.2.3"
	fetcherImageName     = "virt-import-fetcher"
)

func withoutFetcherImage() string {
	return strings.Replace(testManifests, `        env:
        - name: FETCHER_IMAGE
          value: `+renderedFetcherImage+`
`, "", 1)
}

var _ = Describe("csv-generator", func() {
	var manifestsFile string

	BeforeEach(func() {
		manifestsFile = filepath.Join(GinkgoT().TempDir(), "manifests.yaml")
		Expect(os.WriteFile(manifestsFile, []byte(testManifests), 0o600)).To(Succeed())
	})

	validFlags := func() *flags {
		return &flags{
			manifestsFile: manifestsFile,
			csvVersion:    "1.2.3",
			namespace:     "kubevirt",
			operatorImage: operatorImage,
			pullPolicy:    string(corev1.PullAlways),
		}
	}

	Describe("flag validation", func() {
		It("should reject missing required flags, sorted", func() {
			err := (&flags{}).validate()
			Expect(err).To(MatchError(ContainSubstring("[csv-version namespace operator-image]")))
		})

		It("should reject a csv-version that is not semver", func() {
			f := validFlags()
			f.csvVersion = "latest"
			Expect(f.validate()).To(MatchError(ContainSubstring("not a semantic version")))
		})

		It("should accept a complete set of flags", func() {
			Expect(validFlags().validate()).To(Succeed())
		})
	})

	Describe("reading manifests", func() {
		It("should bucket the rendered objects by kind", func() {
			m, err := readManifests(manifestsFile)
			Expect(err).NotTo(HaveOccurred())

			Expect(m.deployment.Name).To(Equal("virt-import-controller"))
			Expect(m.serviceAccounts).To(HaveKey("virt-import-controller-manager"))
			Expect(m.clusterRoles).To(HaveLen(3))
			Expect(m.roles).To(HaveLen(1))
			Expect(m.crds).To(HaveLen(1))
			Expect(m.networkPolicies).To(HaveLen(1))
		})

		It("should fail when there is no Deployment", func() {
			empty := filepath.Join(GinkgoT().TempDir(), "empty.yaml")
			Expect(os.WriteFile(empty, []byte("apiVersion: v1\nkind: Service\n"), 0o600)).To(Succeed())

			_, err := readManifests(empty)
			Expect(err).To(MatchError(ContainSubstring("no Deployment found")))
		})
	})

	Describe("building the CSV", func() {
		var csv *csvv1.ClusterServiceVersion

		BeforeEach(func() {
			m, err := readManifests(manifestsFile)
			Expect(err).NotTo(HaveOccurred())

			csv, err = buildCSV(validFlags(), m)
			Expect(err).NotTo(HaveOccurred())
		})

		strategy := func() *csvv1.StrategyDetailsDeployment {
			return &csv.Spec.InstallStrategy.StrategySpec
		}

		It("should name and place the CSV from the flags", func() {
			Expect(csv.Name).To(Equal("virt-import.v1.2.3"))
			Expect(csv.Namespace).To(Equal("kubevirt"))
			Expect(csv.Spec.Version.String()).To(Equal("1.2.3"))
		})

		It("should merge rules of several bindings to one service account", func() {
			Expect(strategy().ClusterPermissions).To(ConsistOf(And(
				HaveField("ServiceAccountName", "virt-import-controller-manager"),
				HaveField("Rules", HaveLen(2)),
			)))
		})

		It("should skip roles bound to a foreign service account", func() {
			for _, rule := range strategy().ClusterPermissions[0].Rules {
				Expect(rule.Resources).NotTo(ContainElement("pods"))
			}
		})

		It("should carry namespaced bindings as permissions", func() {
			Expect(strategy().Permissions).To(ConsistOf(HaveField("Rules", HaveLen(1))))
		})

		It("should apply the image and pull policy to the manager container", func() {
			Expect(strategy().DeploymentSpecs).To(HaveLen(1))
			container := strategy().DeploymentSpecs[0].Spec.Template.Spec.Containers[0]
			Expect(container.Image).To(Equal(operatorImage))
			Expect(container.ImagePullPolicy).To(Equal(corev1.PullAlways))
		})

		It("should not enable metrics or mount a serving certificate", func() {
			podSpec := strategy().DeploymentSpecs[0].Spec.Template.Spec
			Expect(podSpec.Containers[0].Args).NotTo(ContainElement(ContainSubstring("metrics")))
			Expect(podSpec.Containers[0].VolumeMounts).To(BeEmpty())
			Expect(podSpec.Volumes).To(BeEmpty())
		})

		It("should record the controller and fetcher images as related images", func() {
			Expect(csv.Spec.RelatedImages).To(ConsistOf(
				HaveField("Image", operatorImage),
				csvv1.RelatedImage{Name: fetcherImageName, Image: renderedFetcherImage},
			))
		})
	})

	Describe("the fetcher image", func() {
		managerEnv := func(f *flags, manifests string) ([]corev1.EnvVar, []csvv1.RelatedImage, error) {
			Expect(os.WriteFile(manifestsFile, []byte(manifests), 0o600)).To(Succeed())
			m, err := readManifests(manifestsFile)
			Expect(err).NotTo(HaveOccurred())
			built, err := buildCSV(f, m)
			if err != nil {
				return nil, nil, err
			}
			spec := built.Spec.InstallStrategy.StrategySpec.DeploymentSpecs[0].Spec
			return spec.Template.Spec.Containers[0].Env, built.Spec.RelatedImages, nil
		}

		It("should override the rendered image with --fetcher-image", func() {
			f := validFlags()
			f.fetcherImage = fetcherImage
			env, related, err := managerEnv(f, testManifests)
			Expect(err).NotTo(HaveOccurred())
			Expect(env).To(ConsistOf(corev1.EnvVar{Name: envFetcherImg, Value: fetcherImage}))
			Expect(related).To(ContainElement(csvv1.RelatedImage{Name: fetcherImageName, Image: fetcherImage}))
		})

		It("should fail when the manifests lack FETCHER_IMAGE", func() {
			f := validFlags()
			f.fetcherImage = fetcherImage
			_, _, err := managerEnv(f, withoutFetcherImage())
			Expect(err).To(MatchError(ContainSubstring("has no " + envFetcherImg)))
		})
	})

	Describe("output", func() {
		It("should strip the empty scaffolding of typed round-tripping", func() {
			m, err := readManifests(manifestsFile)
			Expect(err).NotTo(HaveOccurred())

			built, err := buildCSV(validFlags(), m)
			Expect(err).NotTo(HaveOccurred())

			out := &bytes.Buffer{}
			Expect(marshalObject(built, out)).To(Succeed())

			Expect(out.String()).To(HavePrefix("---\n"))
			Expect(out.String()).NotTo(ContainSubstring("status:"))
			Expect(out.String()).NotTo(ContainSubstring("creationTimestamp: null"))
		})

		It("should dump the requested documents verbatim", func() {
			m, err := readManifests(manifestsFile)
			Expect(err).NotTo(HaveOccurred())

			out := &bytes.Buffer{}
			Expect(dumpDocuments(m.crds, out)).To(Succeed())
			Expect(out.String()).To(ContainSubstring("virtualmachineimports.import.kubevirt.io"))

			out.Reset()
			Expect(dumpDocuments(m.networkPolicies, out)).To(Succeed())
			Expect(out.String()).To(ContainSubstring("virt-import-deny-all"))
		})
	})
})
