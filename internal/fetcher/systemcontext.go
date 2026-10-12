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

package fetcher

import (
	"fmt"
	"io/fs"
	"os"
	"strings"

	"go.podman.io/image/v5/types"
)

const (
	certSuffixCRT = ".crt"
	certSuffixPEM = ".pem"
	certFileMode  = 0o600
)

// SystemContext returns registry access settings matching CDI's importer for the same
// credentials, CA ConfigMap and insecure registry setting.
func SystemContext(accessKey, secretKey, certDir, tmpDir string, insecureTLS bool) (*types.SystemContext, error) {
	sys := &types.SystemContext{}
	if accessKey != "" && secretKey != "" {
		sys.DockerAuthConfig = &types.DockerAuthConfig{Username: accessKey, Password: secretKey}
	}
	if certDir != "" {
		collected, err := collectCerts(certDir, tmpDir)
		if err != nil {
			return nil, err
		}
		sys.DockerCertPath = collected
	}
	if insecureTLS {
		sys.DockerInsecureSkipTLSVerify = types.NewOptionalBool(true)
	}
	return sys, nil
}

// collectCerts copies the *.crt and *.pem files of certDir into a new directory, appending .crt
// to every name because containers/image only loads *.crt CA files. Appending, unlike replacing
// the suffix, keeps ca.crt and ca.pem apart.
func collectCerts(certDir, tmpDir string) (string, error) {
	source, err := os.OpenRoot(certDir)
	if err != nil {
		return "", fmt.Errorf("failed to read certificates: %w", err)
	}
	defer source.Close()
	entries, err := fs.ReadDir(source.FS(), ".")
	if err != nil {
		return "", fmt.Errorf("failed to read certificates: %w", err)
	}

	target, err := os.MkdirTemp(tmpDir, "certs-")
	if err != nil {
		return "", err
	}
	targetRoot, err := os.OpenRoot(target)
	if err != nil {
		return "", err
	}
	defer targetRoot.Close()

	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || (!strings.HasSuffix(name, certSuffixCRT) && !strings.HasSuffix(name, certSuffixPEM)) {
			continue
		}
		content, err := source.ReadFile(name)
		if err != nil {
			return "", fmt.Errorf("failed to read certificate %s: %w", name, err)
		}
		if err := targetRoot.WriteFile(name+certSuffixCRT, content, certFileMode); err != nil {
			return "", err
		}
	}

	return target, nil
}
