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
	const logLine = "time=2026-10-09T12:00:00Z level=INFO msg=\"fetching manifest\"\n"

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

	It("should write a single line", func() {
		line := resultLine(newResult(`{"kind":"VirtualMachine"}`))
		Expect(strings.Count(line, "\n")).To(Equal(1))
		Expect(line).To(HavePrefix(`{"fetchResult":{`))
	})

	It("should round trip between other output", func() {
		result := newResult(`{"kind":"VirtualMachine"}`)
		output := logLine + `{"level":"info","msg":"structured log"}` + "\n" + resultLine(result) + logLine
		Expect(artifact.ParseResult(strings.NewReader(output))).To(Equal(result))
	})

	It("should ignore panic output", func() {
		result := newResult(`{"kind":"VirtualMachine"}`)
		output := resultLine(result) + "panic: oops\n\ngoroutine 1 [running]:\nmain.main()\n\t/main.go:1 +0x1\n{ not json\n"
		Expect(artifact.ParseResult(strings.NewReader(output))).To(Equal(result))
	})

	It("should parse a result line longer than the default scanner buffer", func() {
		result := newResult(`{"kind":"VirtualMachine","data":"` + strings.Repeat("x", 512*1024) + `"}`)
		Expect(artifact.ParseResult(strings.NewReader(logLine + resultLine(result)))).To(Equal(result))
	})

	DescribeTable("should fail on", func(output, message string) {
		_, err := artifact.ParseResult(strings.NewReader(output))
		Expect(err).To(MatchError(ContainSubstring(message)))
	},
		Entry("no result", logLine+`{"fetchResult":null}`+"\n", "contains no result"),
		Entry("several results", `{"fetchResult":{}}`+"\n"+`{"fetchResult":{}}`+"\n", "more than one result"),
		Entry("a line over the limit", `{"fetchResult":"`+strings.Repeat("x", 3*1024*1024)+"\n", "failed to read"),
	)
})
