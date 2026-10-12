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
	"syscall"

	"go.podman.io/image/v5/transports"
	"go.podman.io/image/v5/types"

	"kubevirt.io/virt-import/internal/artifact"
	"kubevirt.io/virt-import/internal/fetcher"
)

const terminationMessageFileMode = 0o644

type options struct {
	url, arch, certDir, terminationLog string
	insecureTLS                        bool
}

func main() {
	os.Exit(run())
}

func run() int {
	opts := parseFlags()
	// Logs share stdout with the result line, so they cannot end up inside it.
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// The controller validated the url already, retrying will not make it valid.
	ref, err := fetcher.ParseURL(opts.url)
	if err != nil {
		return fail(logger, opts.terminationLog, artifact.ExitCodeInvalidArtifact, err)
	}
	if err := fetchAndWrite(ctx, logger, ref, opts); err != nil {
		code := artifact.ExitCodeFailed
		if errors.Is(err, artifact.ErrInvalidArtifact) {
			code = artifact.ExitCodeInvalidArtifact
		}
		return fail(logger, opts.terminationLog, code, err)
	}
	return 0
}

func parseFlags() *options {
	opts := &options{}
	flag.StringVar(&opts.url, artifact.FlagURL, "", "docker:// reference of the artifact (required)")
	flag.StringVar(&opts.arch, artifact.FlagArch, "", "architecture to select from a multi-arch artifact")
	flag.StringVar(&opts.certDir, artifact.FlagCertDir, "", "directory with registry CA certificates (*.crt, *.pem)")
	flag.BoolVar(&opts.insecureTLS, artifact.FlagInsecureTLS, false, "skip TLS verification of the registry")
	flag.StringVar(&opts.terminationLog, artifact.FlagTerminationLog, "/dev/termination-log",
		"file the failure reason is written to")
	flag.Parse()
	return opts
}

// fetchAndWrite fetches the artifact metadata and prints the result line.
func fetchAndWrite(ctx context.Context, logger *slog.Logger, ref types.ImageReference, opts *options) error {
	certPath := ""
	if opts.certDir != "" {
		var err error
		// The certificates are copied to the temp dir, so it has to be writable.
		if certPath, err = fetcher.CollectCerts(opts.certDir, os.TempDir()); err != nil {
			return err
		}
	}
	sys := fetcher.SystemContext(os.Getenv(artifact.EnvAccessKeyID), os.Getenv(artifact.EnvSecretKey), certPath,
		opts.insecureTLS)

	src, err := ref.NewImageSource(ctx, sys)
	if err != nil {
		return fmt.Errorf("failed to access %s: %w", transports.ImageName(ref), err)
	}
	defer src.Close()

	logger.Info("Fetching artifact metadata", "url", opts.url, "arch", opts.arch)
	result, err := fetcher.Fetch(ctx, src, opts.arch)
	if err != nil {
		return err
	}
	logger.Info("Fetched artifact metadata", "artifactType", result.ArtifactType, "architecture", result.Architecture,
		"digest", result.Digest, "disks", len(result.Layers))

	return artifact.WriteResult(os.Stdout, result)
}

// fail reports err to the controller through the termination log and returns code.
func fail(logger *slog.Logger, terminationLog string, code int, err error) int {
	logger.Error("Fetching artifact metadata failed", "error", err)
	if writeErr := os.WriteFile(terminationLog, []byte(fetcher.TerminationMessage(err)), terminationMessageFileMode); writeErr != nil {
		logger.Error("Failed to write termination message", "error", writeErr)
	}
	return code
}
