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
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/transport/spdy"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	controllerSelector = "app.kubernetes.io/name=virt-import,control-plane=controller-manager"
	metricsPort        = 8443
)

// Only the NIST curves and AES-GCM cipher suites are checked, since they are
// FIPS-approved and thus guaranteed to be available regardless of whether the
// cluster runs Go's FIPS-restricted crypto.
//
// Each cipher suite set holds the ECDSA and the RSA variant, so the tests do
// not depend on the key type of the serving certificate.
func aes128CipherSuites() []uint16 {
	return []uint16{
		tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
		tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
	}
}

func aes256CipherSuites() []uint16 {
	return []uint16{
		tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,
		tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
	}
}

// The controller manager serves TLS only on its metrics endpoint.
var _ = Describe("TLS", func() {
	DescribeTable(
		"negotiating TLS by default",
		func(config *tls.Config) {
			localPort := forwardControllerPort(metricsPort)
			Expect(dialTLS(localPort, config)).To(Succeed())
		},
		Entry("metrics should negotiate CurveP256", curveConfig(tls.CurveP256)),
		Entry("metrics should negotiate CurveP384", curveConfig(tls.CurveP384)),
		Entry("metrics should negotiate CurveP521", curveConfig(tls.CurveP521)),
		Entry("metrics should negotiate AES-128-GCM", cipherSuitesConfig(aes128CipherSuites())),
		Entry("metrics should negotiate AES-256-GCM", cipherSuitesConfig(aes256CipherSuites())),
	)

	// Serial: these entries restart the controller manager with other args, so
	// they must not run concurrently with any other tests.
	DescribeTable(
		"restricting TLS via deployment args",
		Serial,
		func(flag, value string, accepted, rejected []*tls.Config, rejectedErr string) {
			setControllerArg(flag, value)

			localPort := forwardControllerPort(metricsPort)

			for _, config := range accepted {
				Expect(dialTLS(localPort, config)).To(Succeed())
			}
			for _, config := range rejected {
				Expect(dialTLS(localPort, config)).To(MatchError(ContainSubstring(rejectedErr)))
			}
		},
		Entry("metrics should only accept CurveP256",
			"--tls-curve-preferences", curvePreferencesArg(tls.CurveP256),
			curveConfigs(tls.CurveP256), curveConfigs(tls.CurveP384, tls.CurveP521), "handshake failure"),
		Entry("metrics should only accept CurveP384",
			"--tls-curve-preferences", curvePreferencesArg(tls.CurveP384),
			curveConfigs(tls.CurveP384), curveConfigs(tls.CurveP256, tls.CurveP521), "handshake failure"),
		Entry("metrics should accept CurveP256 and CurveP521 but not CurveP384",
			"--tls-curve-preferences", curvePreferencesArg(tls.CurveP256, tls.CurveP521),
			curveConfigs(tls.CurveP256, tls.CurveP521), curveConfigs(tls.CurveP384), "handshake failure"),
		Entry("metrics should only accept AES-128-GCM",
			"--tls-cipher-suites", cipherSuitesArg(aes128CipherSuites()),
			[]*tls.Config{cipherSuitesConfig(aes128CipherSuites())},
			[]*tls.Config{cipherSuitesConfig(aes256CipherSuites())}, "handshake failure"),
		Entry("metrics should only accept AES-256-GCM",
			"--tls-cipher-suites", cipherSuitesArg(aes256CipherSuites()),
			[]*tls.Config{cipherSuitesConfig(aes256CipherSuites())},
			[]*tls.Config{cipherSuitesConfig(aes128CipherSuites())}, "handshake failure"),
		Entry("metrics should reject TLS 1.2",
			"--tls-min-version", "VersionTLS13",
			[]*tls.Config{versionConfig(tls.VersionTLS13)},
			[]*tls.Config{versionConfig(tls.VersionTLS12)}, "protocol version not supported"),
	)
})

// dialTLS performs a TLS handshake with config against the forwarded port and
// closes the connection. The Go client aborts the handshake if the server
// selects a curve, cipher suite or version that config does not offer, so a
// successful dial proves that one of the offered parameters was negotiated.
func dialTLS(localPort uint16, config *tls.Config) error {
	// The handshake tests negotiation, not certificate validity.
	config = config.Clone()
	config.InsecureSkipVerify = true

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	dialer := &tls.Dialer{Config: config}
	conn, err := dialer.DialContext(ctx, "tcp", fmt.Sprintf("127.0.0.1:%d", localPort))
	if err != nil {
		return err
	}
	return conn.Close()
}

// curveConfig offers only curveID, over TLS 1.3.
func curveConfig(curveID tls.CurveID) *tls.Config {
	return &tls.Config{
		MinVersion:       tls.VersionTLS13,
		CurvePreferences: []tls.CurveID{curveID},
	}
}

// curveConfigs returns one curveConfig per curve.
func curveConfigs(curveIDs ...tls.CurveID) []*tls.Config {
	configs := make([]*tls.Config, len(curveIDs))
	for i, curveID := range curveIDs {
		configs[i] = curveConfig(curveID)
	}
	return configs
}

// cipherSuitesConfig offers only cipherSuites, over TLS 1.2, since TLS 1.3
// cipher suites are not configurable.
func cipherSuitesConfig(cipherSuites []uint16) *tls.Config {
	return &tls.Config{ //nolint:gosec // cipher suites are only configurable up to TLS 1.2
		MinVersion:   tls.VersionTLS12,
		MaxVersion:   tls.VersionTLS12,
		CipherSuites: cipherSuites,
	}
}

// versionConfig offers only version.
func versionConfig(version uint16) *tls.Config {
	return &tls.Config{
		MinVersion: version,
		MaxVersion: version,
	}
}

// curvePreferencesArg formats curveIDs as a --tls-curve-preferences value.
func curvePreferencesArg(curveIDs ...tls.CurveID) string {
	ids := make([]string, len(curveIDs))
	for i, curveID := range curveIDs {
		ids[i] = strconv.Itoa(int(curveID))
	}
	return strings.Join(ids, ",")
}

// cipherSuitesArg formats cipherSuites as a --tls-cipher-suites value.
func cipherSuitesArg(cipherSuites []uint16) string {
	names := make([]string, len(cipherSuites))
	for i, id := range cipherSuites {
		names[i] = tls.CipherSuiteName(id)
	}
	return strings.Join(names, ",")
}

// setControllerArg sets flag to value on the controller manager deployment,
// replacing any existing value, and restores its original arguments when the
// spec finishes.
func setControllerArg(flag, value string) {
	GinkgoHelper()

	deployments, err := kubeClient.AppsV1().Deployments(metav1.NamespaceAll).List(context.Background(), metav1.ListOptions{
		LabelSelector: controllerSelector,
	})
	Expect(err).ToNot(HaveOccurred())
	Expect(deployments.Items).To(HaveLen(1), "expected exactly one controller manager deployment")
	deployment := &deployments.Items[0]
	originalArgs := slices.Clone(deployment.Spec.Template.Spec.Containers[0].Args)
	DeferCleanup(patchDeploymentArgsAndWait, deployment, originalArgs)

	// Drop the flag in both its "--flag value" and "--flag=value" forms.
	args := make([]string, 0, len(originalArgs)+1)
	skipValue := false
	for _, arg := range originalArgs {
		switch {
		case skipValue:
			skipValue = false
		case arg == flag:
			skipValue = true
		case !strings.HasPrefix(arg, flag+"="):
			args = append(args, arg)
		}
	}
	args = append(args, flag+"="+value)

	patchDeploymentArgsAndWait(deployment, args)
}

// patchDeploymentArgsAndWait sets the container args of deployment and waits
// for the rollout. Readiness and the old pod being gone are enough here, since
// nothing but the metrics endpoint is served over TLS.
func patchDeploymentArgsAndWait(deployment *appsv1.Deployment, args []string) {
	GinkgoHelper()

	patch := client.StrategicMergeFrom(deployment.DeepCopy())
	deployment.Spec.Template.Spec.Containers[0].Args = args
	patchBytes, err := patch.Data(deployment)
	Expect(err).ToNot(HaveOccurred())

	deployment, err = kubeClient.AppsV1().Deployments(deployment.Namespace).Patch(
		context.Background(), deployment.Name, patch.Type(), patchBytes, metav1.PatchOptions{},
	)
	Expect(err).ToNot(HaveOccurred())

	replicas := int32(1)
	if deployment.Spec.Replicas != nil {
		replicas = *deployment.Spec.Replicas
	}

	Eventually(func(g Gomega) {
		current, getErr := kubeClient.AppsV1().Deployments(deployment.Namespace).Get(
			context.Background(), deployment.Name, metav1.GetOptions{},
		)
		g.Expect(getErr).ToNot(HaveOccurred())
		g.Expect(current.Status.ObservedGeneration).To(BeNumerically(">=", deployment.Generation))
		g.Expect(current.Status.UpdatedReplicas).To(Equal(replicas))
		g.Expect(current.Status.Replicas).To(Equal(replicas))
		g.Expect(current.Status.AvailableReplicas).To(Equal(replicas))
		// The new pod needs to pass its startup and readiness probes, and the
		// old pod needs to terminate, before the rollout is considered done.
	}, 90*time.Second, 2*time.Second).Should(Succeed())
}

// forwardControllerPort port-forwards to the given port of a running
// controller manager pod and returns the chosen local port.
func forwardControllerPort(remotePort int) (localPort uint16) {
	GinkgoHelper()

	var pod *corev1.Pod
	Eventually(func(g Gomega) *corev1.Pod {
		pods, err := kubeClient.CoreV1().Pods(metav1.NamespaceAll).List(context.Background(), metav1.ListOptions{
			LabelSelector: controllerSelector,
		})
		g.Expect(err).ToNot(HaveOccurred())

		pod = nil
		for i := range pods.Items {
			// A Terminating pod (e.g. the old pod during a rollout) can still
			// report Ready: True until the kubelet actually tears it down, so
			// it must be excluded explicitly.
			if pods.Items[i].DeletionTimestamp != nil {
				continue
			}
			for _, cond := range pods.Items[i].Status.Conditions {
				if cond.Type == corev1.PodReady && cond.Status == corev1.ConditionTrue {
					pod = &pods.Items[i]
					break
				}
			}
			if pod != nil {
				break
			}
		}
		return pod
	}, 30*time.Second, time.Second).ShouldNot(BeNil(), "no ready controller manager pod found")

	roundTripper, upgrader, err := spdy.RoundTripperFor(restConfig)
	Expect(err).ToNot(HaveOccurred())

	req := kubeClient.CoreV1().RESTClient().Post().
		Resource("pods").
		Namespace(pod.Namespace).
		Name(pod.Name).
		SubResource("portforward")
	dialer := spdy.NewDialer(upgrader, &http.Client{Transport: roundTripper}, http.MethodPost, req.URL())

	stopCh := make(chan struct{})
	DeferCleanup(func() { close(stopCh) })
	readyCh := make(chan struct{})
	fw, err := portforward.New(dialer, []string{fmt.Sprintf("0:%d", remotePort)}, stopCh, readyCh, GinkgoWriter, GinkgoWriter)
	Expect(err).ToNot(HaveOccurred())

	go func() {
		defer GinkgoRecover()
		if forwardErr := fw.ForwardPorts(); forwardErr != nil {
			_, _ = fmt.Fprintf(GinkgoWriter, "port-forward stopped: %v\n", forwardErr)
		}
	}()

	Eventually(readyCh, 10*time.Second).Should(BeClosed())

	ports, err := fw.GetPorts()
	Expect(err).ToNot(HaveOccurred())
	Expect(ports).ToNot(BeEmpty())
	localPort = ports[0].Local

	var tcpDialer net.Dialer
	Eventually(func() error {
		conn, dialErr := tcpDialer.DialContext(context.Background(), "tcp", fmt.Sprintf("127.0.0.1:%d", localPort))
		if dialErr != nil {
			return dialErr
		}
		return conn.Close()
	}, 30*time.Second, time.Second).Should(Succeed())

	return localPort
}
