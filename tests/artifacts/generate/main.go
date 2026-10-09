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

// Command generate writes the test artifact fixtures as oci-archive tarballs.
package main

import (
	"bytes"
	"flag"
	"log"
	"os"
	"path/filepath"

	"kubevirt.io/virt-import/internal/testing/artifacts"
)

const fileMode = 0o644

func main() {
	outDir := flag.String("out-dir", "tests/artifacts", "directory to write the fixtures to")
	flag.Parse()

	for name, fixture := range artifacts.Fixtures() {
		buf := &bytes.Buffer{}
		if err := fixture.WriteArchive(buf); err != nil {
			log.Fatalf("failed to build fixture %s: %v", name, err)
		}
		if err := os.WriteFile(filepath.Join(*outDir, name+".tar"), buf.Bytes(), fileMode); err != nil {
			log.Fatalf("failed to write fixture %s: %v", name, err)
		}
	}
}
