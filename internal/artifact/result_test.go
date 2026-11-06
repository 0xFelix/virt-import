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

package artifact_test

import (
	"bytes"
	"encoding/json"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/opencontainers/go-digest"

	"kubevirt.io/virt-import/internal/artifact"
)

var _ = Describe("FetchResult", func() {
	newResult := func(config string) *artifact.FetchResult {
		return &artifact.FetchResult{
			ArtifactType:   artifact.ArtifactTypeVM,
			Architecture:   "amd64",
			IsIndex:        true,
			Digest:         digest.FromString("index"),
			ManifestDigest: digest.FromString("manifest"),
			Config:         json.RawMessage(config),
			Layers: []artifact.Layer{
				{Digest: digest.FromString("rootdisk"), DiskName: "rootdisk", DiskSize: "10Gi"},
			},
		}
	}

	resultLine := func(result *artifact.FetchResult) string {
		buf := &bytes.Buffer{}
		ExpectWithOffset(1, artifact.WriteResult(buf, result)).To(Succeed())
		return buf.String()
	}

	decode := func(line string) *artifact.FetchResult {
		parsed := struct {
			FetchResult *artifact.FetchResult `json:"fetchResult"`
			Version     int                   `json:"version"`
		}{}
		ExpectWithOffset(1, json.Unmarshal([]byte(line), &parsed)).To(Succeed())
		ExpectWithOffset(1, parsed.Version).To(Equal(1))
		return parsed.FetchResult
	}

	It("should write the result as a single line", func() {
		result := newResult(`{"kind":"VirtualMachine"}`)
		line := resultLine(result)
		Expect(strings.Count(line, "\n")).To(Equal(1))
		Expect(line).To(HavePrefix(`{"fetchResult":{`))
		Expect(decode(line)).To(Equal(result))
	})

	It("should not escape characters in the config that need no escaping", func() {
		// Escaped, a config this large would exceed the line limit.
		result := newResult(`{"kind":"VirtualMachine","data":"` + strings.Repeat("<&>", 512*1024/3) + `"}`)
		line := resultLine(result)
		Expect(line).To(ContainSubstring("<&>"))
		Expect(decode(line)).To(Equal(result))
	})

	It("should reject a result over the line limit", func() {
		result := newResult(`{"kind":"VirtualMachine","data":"` + strings.Repeat("x", 3*1024*1024) + `"}`)
		Expect(artifact.WriteResult(&bytes.Buffer{}, result)).To(MatchError(artifact.ErrInvalidArtifact))
	})
})
