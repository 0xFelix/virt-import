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

package virtctl_test

import (
	"bytes"
	"errors"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/spf13/cobra"

	virtctl "kubevirt.io/virt-import/internal/virtimportctl"
	"kubevirt.io/virt-import/internal/virtimportctl/testing"
)

var _ = Describe("virtctl", func() {
	DescribeTable("GetProgramName", func(binary, expected string) {
		Expect(virtctl.GetProgramName(binary)).To(Equal(expected))
	},
		Entry("returns virtimportctl", "virtimportctl", "virtimportctl"),
		Entry("returns virtimportctl as default", "42", "virtimportctl"),
		Entry("returns kubectl", "kubectl-virtimport", "kubectl virtimport"),
		Entry("returns oc", "oc-virtimport", "oc virtimport"),
	)

	DescribeTable("the log verbosity flag should be supported", func(arg string) {
		Expect(testing.NewRepeatableVirtctlCommand(arg)()).To(Succeed())
	},
		Entry("regular flag", "--v=2"),
		Entry("shorthand flag", "-v=2"),
	)

	It("Execute should print the error and fail", func() {
		const testError = "testError"
		cmd := &cobra.Command{
			RunE: func(_ *cobra.Command, _ []string) error {
				return errors.New(testError)
			},
		}
		out := &bytes.Buffer{}
		cmd.SetErr(out)

		virtctl.NewVirtctlCommand = func() *cobra.Command {
			return cmd
		}
		DeferCleanup(func() {
			virtctl.NewVirtctlCommand = virtctl.NewVirtctlCommandFn
		})

		Expect(virtctl.Execute()).To(Equal(1))
		Expect(out.String()).To(ContainSubstring(testError))
	})
})
