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

package controller_test

import (
	"context"
	"path/filepath"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"kubevirt.io/virt-import-api/core/v1alpha1"

	"kubevirt.io/virt-import/internal/controller"
)

var _ = Describe("AvailabilityController", func() {
	var (
		env *envtest.Environment
		ac  *controller.AvailabilityController
	)

	startController := func() (chan error, context.CancelFunc) {
		ctrlCtx, ctrlCancel := context.WithCancel(ctx)
		done := make(chan error, 1)
		go func() {
			done <- ac.Start(ctrlCtx)
		}()
		return done, ctrlCancel
	}

	BeforeEach(func() {
		env = startNoCRDEnv()
		ac = &controller.AvailabilityController{
			Manager:         startManager(env.Config),
			DiscoveryClient: discovery.NewDiscoveryClientForConfigOrDie(env.Config),
		}
		ac.SetPollInterval(1 * time.Second)
	})

	It("should start the import controller once required CRDs become available", func() {
		done, ctrlCancel := startController()
		DeferCleanup(ctrlCancel)
		Consistently(done, 2*time.Second).ShouldNot(Receive())

		_, err := envtest.InstallCRDs(env.Config, envtest.CRDInstallOptions{
			Paths: []string{
				filepath.Join("..", "..", "config", "crd", "testing"),
			},
			Scheme: testScheme,
		})
		Expect(err).ToNot(HaveOccurred())

		Eventually(done, 60*time.Second).Should(Receive(BeNil()))
	})

	It("should set status on pending VirtualMachineImports while waiting", func() {
		cl, err := client.New(env.Config, client.Options{Scheme: testScheme})
		Expect(err).NotTo(HaveOccurred())

		Expect(cl.Create(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: testNamespace},
		})).To(Succeed())

		vmImport := &v1alpha1.VirtualMachineImport{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-import",
				Namespace: testNamespace,
			},
			Spec: v1alpha1.VirtualMachineImportSpec{
				Source: v1alpha1.VirtualMachineImportSource{
					Registry: &v1alpha1.VirtualMachineImportRegistrySource{
						URL: "docker://registry.example.com/vms/fedora:v1",
					},
				},
			},
		}
		Expect(cl.Create(ctx, vmImport)).To(Succeed())

		done, ctrlCancel := startController()
		DeferCleanup(ctrlCancel)

		Eventually(func(g Gomega) {
			g.Expect(cl.Get(ctx, client.ObjectKeyFromObject(vmImport), vmImport)).To(Succeed())

			ready := meta.FindStatusCondition(vmImport.Status.Conditions, v1alpha1.ConditionReady)
			g.Expect(ready).ToNot(BeNil())
			g.Expect(ready.Status).To(Equal(metav1.ConditionFalse))
			g.Expect(ready.Reason).To(Equal(v1alpha1.ReasonWaiting))
			g.Expect(ready.Message).To(ContainSubstring("Required CRDs are not yet available"))

			progressing := meta.FindStatusCondition(vmImport.Status.Conditions, v1alpha1.ConditionProgressing)
			g.Expect(progressing).ToNot(BeNil())
			g.Expect(progressing.Status).To(Equal(metav1.ConditionTrue))
			g.Expect(progressing.Reason).To(Equal(v1alpha1.ReasonWaiting))
		}, 60*time.Second).Should(Succeed())

		ctrlCancel()
		Eventually(done, 60*time.Second).Should(Receive(BeNil()))
	})

	It("should stop polling when context is canceled", func() {
		done, ctrlCancel := startController()
		Consistently(done, 2*time.Second).ShouldNot(Receive())
		ctrlCancel()
		Eventually(done, 60*time.Second).Should(Receive(BeNil()))
	})
})

// startNoCRDEnv starts an environment with only the virt-import CRD installed,
// so the external CRDs the controller waits for are deliberately absent.
func startNoCRDEnv() *envtest.Environment {
	env := &envtest.Environment{
		CRDDirectoryPaths: []string{
			filepath.Join("..", "..", "config", "crd", "bases"),
		},
		ErrorIfCRDPathMissing: true,
		Scheme:                testScheme,
	}
	if dir := getFirstFoundEnvTestBinaryDir(); dir != "" {
		env.BinaryAssetsDirectory = dir
	}

	_, err := env.Start()
	Expect(err).NotTo(HaveOccurred())
	DeferCleanup(func() {
		Expect(env.Stop()).To(Succeed())
	})

	return env
}

func startManager(cfg *rest.Config) ctrl.Manager {
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme: testScheme,
	})
	Expect(err).NotTo(HaveOccurred())

	mgrCtx, mgrCancel := context.WithCancel(ctx)
	DeferCleanup(mgrCancel)
	go func() {
		defer GinkgoRecover()
		Expect(mgr.Start(mgrCtx)).To(Succeed())
	}()

	return mgr
}
