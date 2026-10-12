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
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"go.podman.io/image/v5/docker"

	"kubevirt.io/virt-import/internal/artifact"
	"kubevirt.io/virt-import/internal/fetcher"
)

const terminationMessageFileMode = 0o644

func main() {
	os.Exit(run())
}

func run() int {
	var url, arch, certDir, terminationLog string
	var insecureTLS bool
	flag.StringVar(&url, artifact.FlagURL, "", "docker:// reference of the artifact (required)")
	flag.StringVar(&arch, artifact.FlagArch, "", "architecture to select from a multi-arch artifact")
	flag.StringVar(&certDir, artifact.FlagCertDir, "", "directory with registry CA certificates (*.crt, *.pem)")
	flag.BoolVar(&insecureTLS, artifact.FlagInsecureTLS, false, "skip TLS verification of the registry")
	flag.StringVar(&terminationLog, artifact.FlagTerminationLog, "/dev/termination-log", "file the failure reason is written to")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	fail := func(code int, err error) int {
		logger.Error("Fetching artifact metadata failed", "error", err)
		//nolint:gosec // The path is a flag set by the controller.
		if writeErr := os.WriteFile(terminationLog, []byte(err.Error()), terminationMessageFileMode); writeErr != nil {
			logger.Error("Failed to write termination message", "error", writeErr)
		}
		return code
	}

	name, ok := strings.CutPrefix(url, artifact.DockerScheme)
	if !ok {
		return fail(artifact.ExitCodeFailed, fmt.Errorf("url %q has to start with %s", url, artifact.DockerScheme))
	}
	ref, err := docker.ParseReference("//" + name)
	if err != nil {
		return fail(artifact.ExitCodeFailed, fmt.Errorf("invalid url %q: %w", url, err))
	}
	// The certificates are copied to the temp dir, so it has to be writable.
	sys, err := fetcher.SystemContext(os.Getenv(artifact.EnvAccessKeyID), os.Getenv(artifact.EnvSecretKey), certDir,
		os.TempDir(), insecureTLS)
	if err != nil {
		return fail(artifact.ExitCodeFailed, err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	logger.Info("Fetching artifact metadata", "url", url, "arch", arch)
	result, err := fetcher.Fetch(ctx, ref, sys, arch)
	if err != nil {
		code := artifact.ExitCodeFailed
		if errors.Is(err, artifact.ErrInvalidArtifact) {
			code = artifact.ExitCodeInvalidArtifact
		}
		return fail(code, err)
	}

	logger.Info("Fetched artifact metadata", "artifactType", result.ArtifactType, "architecture", result.Architecture,
		"digest", result.Digest, "disks", len(result.Layers))
	// Write the result last. Runtimes split long lines and pod logs merge stdout and stderr, so a
	// log line written after it could end up inside it.
	if err := artifact.WriteResult(os.Stdout, result); err != nil {
		return fail(artifact.ExitCodeFailed, err)
	}
	return 0
}
