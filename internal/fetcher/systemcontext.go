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
	"errors"
	"fmt"
	"io"
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

// SystemContext returns the registry settings CDI's importer uses for the same credentials,
// certificates and insecure registry setting. certPath comes from CollectCerts, or is empty.
func SystemContext(accessKey, secretKey, certPath string, insecureTLS bool) *types.SystemContext {
	sys := &types.SystemContext{DockerCertPath: certPath}
	if accessKey != "" && secretKey != "" {
		sys.DockerAuthConfig = &types.DockerAuthConfig{Username: accessKey, Password: secretKey}
	}
	if insecureTLS {
		sys.DockerInsecureSkipTLSVerify = types.NewOptionalBool(true)
	}
	return sys
}

// CollectCerts copies the *.crt and *.pem files of certDir to a new directory under tmpDir and
// returns its path. containers/image only loads *.crt files, so .crt is appended to every name.
// Appending instead of replacing keeps ca.crt and ca.pem apart.
func CollectCerts(certDir, tmpDir string) (_ string, retErr error) {
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
	defer func() {
		if retErr != nil {
			retErr = errors.Join(retErr, os.RemoveAll(target))
		}
	}()
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
		if err := copyCert(source, targetRoot, name); err != nil {
			return "", err
		}
	}

	return target, nil
}

// copyCert copies name from source to target as name.crt.
func copyCert(source, target *os.Root, name string) error {
	in, err := source.Open(name)
	if err != nil {
		return fmt.Errorf("failed to read certificate %s: %w", name, err)
	}
	defer in.Close()

	out, err := target.OpenFile(name+certSuffixCRT, os.O_WRONLY|os.O_CREATE|os.O_EXCL, certFileMode)
	if err != nil {
		return err
	}
	// Copying between files lets the kernel copy the content.
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return fmt.Errorf("failed to copy certificate %s: %w", name, err)
	}
	return out.Close()
}
