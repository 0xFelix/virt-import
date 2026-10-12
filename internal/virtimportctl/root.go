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

package virtctl

import (
	"context"
	"flag"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"kubevirt.io/client-go/log"

	"kubevirt.io/virt-import/internal/virtimportctl/clientconfig"
	"kubevirt.io/virt-import/internal/virtimportctl/create"
	"kubevirt.io/virt-import/internal/virtimportctl/kubecli"
	"kubevirt.io/virt-import/internal/virtimportctl/templates"
)

var (
	NewVirtctlCommand = NewVirtctlCommandFn

	programName = GetProgramName(filepath.Base(os.Args[0]))
)

// GetProgramName returns the command name to display in help texts.
// If `virtimportctl` is installed via krew to be used as a kubectl plugin, it's run via a symlink, so the basename then
// is `kubectl-virtimport`. In this case we want to accommodate the user by adjusting the help text (usage, examples and
// the like) by displaying `kubectl virtimport <command>` instead of `virtimportctl <command>`.
// see https://github.com/kubevirt/kubevirt/issues/2356 for more details
// see also templates.go
func GetProgramName(binary string) string {
	if strings.HasSuffix(binary, "-virtimport") {
		return strings.TrimSuffix(binary, "-virtimport") + " virtimport"
	}
	return "virtimportctl"
}

func NewVirtctlCommandFn() *cobra.Command {
	// used in cobra templates to display either `kubectl virtimport` or `virtimportctl`
	cobra.AddTemplateFunc(
		"ProgramName", func() string {
			return programName
		},
	)

	// used to enable replacement of `ProgramName` placeholder for cobra.Example, which has no template support
	cobra.AddTemplateFunc(
		"prepare", func(s string) string {
			return strings.Replace(s, "{{ProgramName}}", programName, -1) //nolint:staticcheck // Kept as in virtctl.
		},
	)

	optionsCmd := &cobra.Command{
		Use:    "options",
		Hidden: true,
		Run: func(cmd *cobra.Command, _ []string) {
			cmd.Printf("%s", cmd.UsageString())
		},
	}
	optionsCmd.SetUsageTemplate(templates.OptionsUsageTemplate())

	rootCmd := &cobra.Command{
		Use:           programName,
		Short:         programName + " imports virtual machines and templates from OCI artifacts into your kubernetes cluster.",
		SilenceUsage:  true,
		SilenceErrors: true,
		Run: func(cmd *cobra.Command, _ []string) {
			cmd.Printf("%s", cmd.UsageString())
		},
	}
	addVerbosityFlag(rootCmd.PersistentFlags())
	rootCmd.SetUsageTemplate(templates.MainUsageTemplate())
	rootCmd.SetOut(os.Stdout)
	rootCmd.SetContext(clientconfig.NewContext(
		context.Background(), kubecli.DefaultClientConfig(rootCmd.PersistentFlags()),
	))

	rootCmd.AddCommand(
		create.NewCommand(),
		optionsCmd,
	)

	return rootCmd
}

func addVerbosityFlag(fs *pflag.FlagSet) {
	// The verbosity flag is added to the default flag set
	// by init() in kubevirt.io/client-go/log/log.go.
	// We re-add it here to make it available in virtimportctl commands.
	// log.VerbosityFlag() replaces this lookup in newer kubevirt.io/client-go.
	if f := flag.CommandLine.Lookup("v"); f != nil {
		fs.AddFlag(pflag.PFlagFromGoFlag(f))
	} else {
		panic("failed to find verbosity flag \"v\" in default flag set")
	}
}

func Execute() int {
	log.InitializeLogging(programName)
	cmd := NewVirtctlCommand()
	if err := cmd.Execute(); err != nil {
		cmd.PrintErrln(err)
		return 1
	}
	return 0
}
