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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"

	importapi "kubevirt.io/virt-import-api/core"
	importclient "kubevirt.io/virt-import-client-go/virtimport"

	"kubevirt.io/virt-import/internal/artifact/artifacttest"
)

var _ = Describe("VirtualMachineImport Authorization", func() {
	const (
		vmImportAuthzTest  = "vmimport-authz-test-"
		roleKind           = "Role"
		verbCreate         = "create"
		cdiAPIGroup        = "cdi.kubevirt.io"
		kubevirtAPIGroup   = "kubevirt.io"
		templateAPIGroup   = "template.kubevirt.io"
		dataVolumes        = "datavolumes"
		virtualMachines    = "virtualmachines"
		templatesResources = "virtualmachinetemplates"
	)

	var serviceAccount *corev1.ServiceAccount

	BeforeEach(func() {
		serviceAccount = &corev1.ServiceAccount{
			ObjectMeta: metav1.ObjectMeta{
				Namespace:    testNamespace,
				GenerateName: vmImportAuthzTest,
			},
		}
		var err error
		serviceAccount, err = kubeClient.CoreV1().ServiceAccounts(testNamespace).
			Create(context.Background(), serviceAccount, metav1.CreateOptions{})
		Expect(err).ToNot(HaveOccurred())
	})

	createRole := func(rules ...rbacv1.PolicyRule) *rbacv1.Role {
		GinkgoHelper()
		role := &rbacv1.Role{
			ObjectMeta: metav1.ObjectMeta{
				Namespace:    testNamespace,
				GenerateName: vmImportAuthzTest,
			},
			Rules: rules,
		}
		role, err := kubeClient.RbacV1().Roles(testNamespace).Create(context.Background(), role, metav1.CreateOptions{})
		Expect(err).ToNot(HaveOccurred())
		return role
	}

	createRule := func(apiGroup, resource string) rbacv1.PolicyRule {
		return rbacv1.PolicyRule{APIGroups: []string{apiGroup}, Resources: []string{resource}, Verbs: []string{verbCreate}}
	}

	// waitForPermission blocks until the RBAC cache reflects the expected permission for the service account.
	waitForPermission := func(apiGroup, resource string) {
		GinkgoHelper()
		userID := "system:serviceaccount:" + serviceAccount.Namespace + ":" + serviceAccount.Name
		Eventually(func(g Gomega) {
			sar := &authorizationv1.SubjectAccessReview{
				Spec: authorizationv1.SubjectAccessReviewSpec{
					User: userID,
					ResourceAttributes: &authorizationv1.ResourceAttributes{
						Namespace: testNamespace,
						Verb:      verbCreate,
						Group:     apiGroup,
						Resource:  resource,
					},
				},
			}
			result, err := kubeClient.AuthorizationV1().SubjectAccessReviews().Create(context.Background(), sar, metav1.CreateOptions{})
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(result.Status.Allowed).To(BeTrue())
		}, 30*time.Second, 1*time.Second).Should(Succeed())
	}

	createRoleBinding := func(role *rbacv1.Role) {
		GinkgoHelper()
		rb := &rbacv1.RoleBinding{
			ObjectMeta: metav1.ObjectMeta{
				Namespace:    testNamespace,
				GenerateName: vmImportAuthzTest,
			},
			Subjects: []rbacv1.Subject{
				{
					Kind:      rbacv1.ServiceAccountKind,
					Name:      serviceAccount.Name,
					Namespace: serviceAccount.Namespace,
				},
			},
			RoleRef: rbacv1.RoleRef{
				APIGroup: rbacv1.GroupName,
				Kind:     roleKind,
				Name:     role.Name,
			},
		}
		_, err := kubeClient.RbacV1().RoleBindings(testNamespace).Create(context.Background(), rb, metav1.CreateOptions{})
		Expect(err).ToNot(HaveOccurred())
		for _, rule := range role.Rules {
			waitForPermission(rule.APIGroups[0], rule.Resources[0])
		}
	}

	createImportAsServiceAccount := func() error {
		GinkgoHelper()
		cfg := rest.CopyConfig(restConfig)
		cfg.Impersonate = rest.ImpersonationConfig{
			UserName: "system:serviceaccount:" + serviceAccount.Namespace + ":" + serviceAccount.Name,
		}
		saClient, err := importclient.NewForConfig(cfg)
		Expect(err).ToNot(HaveOccurred())

		_, err = saClient.ImportV1alpha1().VirtualMachineImports(testNamespace).
			Create(context.Background(), newImport(fixtureURL(artifacttest.VMSingle)), metav1.CreateOptions{})
		return err
	}

	DescribeTable("should deny when user cannot create", func(missing string, rules ...rbacv1.PolicyRule) {
		createRoleBinding(createRole(append(rules, createRule(importapi.GroupName, importapi.PluralResourceName))...))

		Expect(createImportAsServiceAccount()).
			To(MatchError(ContainSubstring("User is not allowed to create " + missing + " in namespace " + testNamespace)))
	},
		Entry("DataVolumes", "DataVolumes",
			createRule(kubevirtAPIGroup, virtualMachines), createRule(templateAPIGroup, templatesResources)),
		Entry("VirtualMachines", "VirtualMachines",
			createRule(cdiAPIGroup, dataVolumes), createRule(templateAPIGroup, templatesResources)),
		Entry("VirtualMachineTemplates", "VirtualMachineTemplates",
			createRule(cdiAPIGroup, dataVolumes), createRule(kubevirtAPIGroup, virtualMachines)),
	)

	It("should allow when user can create all possible targets and DataVolumes", func() {
		createRoleBinding(createRole(
			createRule(importapi.GroupName, importapi.PluralResourceName),
			createRule(cdiAPIGroup, dataVolumes),
			createRule(kubevirtAPIGroup, virtualMachines),
			createRule(templateAPIGroup, templatesResources),
		))

		Expect(createImportAsServiceAccount()).To(Succeed())
	})
})
