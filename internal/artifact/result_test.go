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

	It("should parse the result between other output", func() {
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

	It("should not escape characters in the config that need no escaping", func() {
		// Escaped, a config this large would exceed the line limit.
		result := newResult(`{"kind":"VirtualMachine","data":"` + strings.Repeat("<&>", 512*1024/3) + `"}`)
		line := resultLine(result)
		Expect(line).To(ContainSubstring("<&>"))
		Expect(artifact.ParseResult(strings.NewReader(line))).To(Equal(result))
	})

	It("should reject a result over the line limit", func() {
		result := newResult(`{"kind":"VirtualMachine","data":"` + strings.Repeat("x", 3*1024*1024) + `"}`)
		Expect(artifact.WriteResult(&bytes.Buffer{}, result)).To(MatchError(artifact.ErrInvalidArtifact))
	})

	DescribeTable("should fail on", func(output, message string) {
		_, err := artifact.ParseResult(strings.NewReader(output))
		Expect(err).To(MatchError(ContainSubstring(message)))
	},
		Entry("no result", logLine+`{"level":"info","msg":"structured log"}`+"\n", "contains no result"),
		Entry("a null result", logLine+`{"fetchResult":null}`+"\n", "holds no result"),
		Entry("a malformed result", `{"fetchResult":{"artifactType":`+"\n", "invalid result line"),
		Entry("an unsupported version", `{"fetchResult":{},"version":2}`+"\n", "unsupported result version 2"),
		Entry("a missing version", `{"fetchResult":{}}`+"\n", "unsupported result version 0"),
		Entry("several results", `{"fetchResult":{},"version":1}`+"\n"+`{"fetchResult":{},"version":1}`+"\n",
			"more than one result"),
		Entry("a line over the limit", `{"fetchResult":{"artifactType":"`+strings.Repeat("x", 3*1024*1024)+`"}}`+"\n",
			"failed to read"),
	)
})
