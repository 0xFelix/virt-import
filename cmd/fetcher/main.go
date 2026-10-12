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

const (
	dockerScheme = "docker://"
	// docker.ParseReference wants the reference with its leading "//".
	dockerTransport = "docker:"

	// Registry credentials, the same keys CDI reads from the Secret referenced by a DataVolume.
	envAccessKeyID = "ACCESS_KEY_ID"
	envSecretKey   = "SECRET_KEY"

	terminationMessageFileMode = 0o644
)

func main() {
	os.Exit(run())
}

func run() int {
	var url, arch, certDir, terminationLog string
	var insecureTLS bool
	flag.StringVar(&url, "url", "", "docker:// reference of the artifact (required)")
	flag.StringVar(&arch, "arch", "", "architecture to select from a multi-arch artifact")
	flag.StringVar(&certDir, "cert-dir", "", "directory with registry CA certificates (*.crt, *.pem)")
	flag.BoolVar(&insecureTLS, "insecure-tls", false, "skip TLS verification of the registry")
	flag.StringVar(&terminationLog, "termination-log", "/dev/termination-log", "file the failure reason is written to")
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

	if !strings.HasPrefix(url, dockerScheme) {
		return fail(artifact.ExitCodeFailed, fmt.Errorf("url %q has to start with %s", url, dockerScheme))
	}
	ref, err := docker.ParseReference(strings.TrimPrefix(url, dockerTransport))
	if err != nil {
		return fail(artifact.ExitCodeFailed, fmt.Errorf("invalid url %q: %w", url, err))
	}
	sys, err := fetcher.SystemContext(os.Getenv(envAccessKeyID), os.Getenv(envSecretKey), certDir, os.TempDir(), insecureTLS)
	if err != nil {
		return fail(artifact.ExitCodeFailed, err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	logger.Info("Fetching artifact metadata", "url", url, "arch", arch)
	result, err := fetcher.Fetch(ctx, ref, sys, arch)
	if errors.Is(err, artifact.ErrInvalidArtifact) {
		return fail(artifact.ExitCodeInvalidArtifact, err)
	} else if err != nil {
		return fail(artifact.ExitCodeFailed, err)
	}

	if err := artifact.WriteResult(os.Stdout, result); err != nil {
		return fail(artifact.ExitCodeFailed, err)
	}
	logger.Info("Fetched artifact metadata", "artifactType", result.ArtifactType, "architecture", result.Architecture,
		"digest", result.Digest, "disks", len(result.Layers))
	return 0
}
