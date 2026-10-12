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

package fetcher_test

import (
	"os"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"go.podman.io/image/v5/types"

	"kubevirt.io/virt-import/internal/fetcher"
)

var _ = Describe("SystemContext", func() {
	const (
		accessKey = "user"
		secretKey = "password"
		fileMode  = 0o600
		ignored   = "ignored"

		caCRT      = "ca.crt"
		otherPEM   = "other.pem"
		contentCRT = "crt"
		contentPEM = "pem"
	)

	It("should be empty without settings", func() {
		Expect(fetcher.SystemContext("", "", "", GinkgoT().TempDir(), false)).To(Equal(&types.SystemContext{}))
	})

	DescribeTable("should set credentials", func(access, secret string, expected *types.DockerAuthConfig) {
		sys, err := fetcher.SystemContext(access, secret, "", GinkgoT().TempDir(), false)
		Expect(err).ToNot(HaveOccurred())
		Expect(sys.DockerAuthConfig).To(Equal(expected))
	},
		Entry("when both keys are set", accessKey, secretKey, &types.DockerAuthConfig{Username: accessKey, Password: secretKey}),
		Entry("not with the access key only", accessKey, "", nil),
		Entry("not with the secret key only", "", secretKey, nil),
	)

	It("should skip TLS verification of insecure registries", func() {
		sys, err := fetcher.SystemContext("", "", "", GinkgoT().TempDir(), true)
		Expect(err).ToNot(HaveOccurred())
		Expect(sys.DockerInsecureSkipTLSVerify).To(Equal(types.OptionalBoolTrue))
	})

	certNames := func(sys *types.SystemContext) []string {
		entries, err := os.ReadDir(sys.DockerCertPath)
		ExpectWithOffset(1, err).ToNot(HaveOccurred())
		names := []string{}
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		return names
	}

	It("should collect *.crt and *.pem certificates under distinct *.crt names", func() {
		certDir := GinkgoT().TempDir()
		for name, content := range map[string]string{
			caCRT:     contentCRT,
			"ca.pem":  contentPEM,
			otherPEM:  "other",
			"key.key": ignored,
			"README":  ignored,
		} {
			Expect(os.WriteFile(filepath.Join(certDir, name), []byte(content), fileMode)).To(Succeed())
		}
		Expect(os.Mkdir(filepath.Join(certDir, "..data.crt"), 0o700)).To(Succeed())

		sys, err := fetcher.SystemContext("", "", certDir, GinkgoT().TempDir(), false)
		Expect(err).ToNot(HaveOccurred())
		Expect(certNames(sys)).To(ConsistOf("ca.crt.crt", "ca.pem.crt", "other.pem.crt"))
		Expect(os.ReadFile(filepath.Join(sys.DockerCertPath, "ca.crt.crt"))).To(Equal([]byte(contentCRT)))
		Expect(os.ReadFile(filepath.Join(sys.DockerCertPath, "ca.pem.crt"))).To(Equal([]byte(contentPEM)))
	})

	It("should collect certificates from a mounted ConfigMap", func() {
		// A ConfigMap volume links each key through ..data to a timestamped directory.
		const dataDir = "..2026_10_14_00_00_00.000000000"
		certDir := GinkgoT().TempDir()
		Expect(os.Mkdir(filepath.Join(certDir, dataDir), 0o700)).To(Succeed())
		Expect(os.Symlink(dataDir, filepath.Join(certDir, "..data"))).To(Succeed())
		for name, content := range map[string]string{caCRT: contentCRT, otherPEM: contentPEM} {
			Expect(os.WriteFile(filepath.Join(certDir, dataDir, name), []byte(content), fileMode)).To(Succeed())
			Expect(os.Symlink(filepath.Join("..data", name), filepath.Join(certDir, name))).To(Succeed())
		}

		sys, err := fetcher.SystemContext("", "", certDir, GinkgoT().TempDir(), false)
		Expect(err).ToNot(HaveOccurred())
		Expect(certNames(sys)).To(ConsistOf("ca.crt.crt", "other.pem.crt"))
		Expect(os.ReadFile(filepath.Join(sys.DockerCertPath, "ca.crt.crt"))).To(Equal([]byte(contentCRT)))
		Expect(os.ReadFile(filepath.Join(sys.DockerCertPath, "other.pem.crt"))).To(Equal([]byte(contentPEM)))
	})

	It("should not follow a certificate symlink out of the certificate directory", func() {
		outside := filepath.Join(GinkgoT().TempDir(), "outside.crt")
		Expect(os.WriteFile(outside, []byte(contentCRT), fileMode)).To(Succeed())
		certDir := GinkgoT().TempDir()
		Expect(os.Symlink(outside, filepath.Join(certDir, caCRT))).To(Succeed())

		_, err := fetcher.SystemContext("", "", certDir, GinkgoT().TempDir(), false)
		Expect(err).To(MatchError(ContainSubstring("failed to read certificate ca.crt")))
	})

	It("should fail on a missing certificate directory", func() {
		_, err := fetcher.SystemContext("", "", filepath.Join(GinkgoT().TempDir(), "missing"), GinkgoT().TempDir(), false)
		Expect(err).To(MatchError(ContainSubstring("failed to read certificates")))
	})
})
