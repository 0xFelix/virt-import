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

// Package kubecli holds the parts of kubevirt.io/client-go/kubecli that the modules taken from
// virtctl use. kubecli cannot be imported while it does not support Kubernetes 0.37. Replace the
// imports of this package with kubevirt.io/client-go/kubecli once it does, and remove it.
package kubecli

import (
	"os"

	"github.com/spf13/pflag"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// KubevirtClient is the subset of kubecli.KubevirtClient virtimportctl needs.
type KubevirtClient interface {
	Config() *rest.Config
}

type kubevirtClient struct {
	config *rest.Config
}

func (k *kubevirtClient) Config() *rest.Config {
	return k.config
}

// DefaultClientConfig is a copy of kubecli.DefaultClientConfig.
func DefaultClientConfig(flags *pflag.FlagSet) clientcmd.ClientConfig {
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	// use the standard defaults for this client command
	// DEPRECATED: remove and replace with something more accurate
	loadingRules.DefaultClientConfig = &clientcmd.DefaultClientConfig

	flags.StringVar(&loadingRules.ExplicitPath, "kubeconfig", "", "Path to the kubeconfig file to use for CLI requests.")

	overrides := &clientcmd.ConfigOverrides{ClusterDefaults: clientcmd.ClusterDefaults}

	flagNames := clientcmd.RecommendedConfigOverrideFlags("")
	// short flagnames are disabled by default.  These are here for compatibility with existing scripts
	flagNames.ClusterOverrideFlags.APIServer.ShortName = "s"

	clientcmd.BindOverrideFlags(overrides, flags, flagNames)
	clientConfig := clientcmd.NewInteractiveDeferredLoadingClientConfig(loadingRules, overrides, os.Stdin)

	return clientConfig
}

// GetKubevirtClientFromClientConfig mirrors kubecli.GetKubevirtClientFromClientConfig.
// It is a variable so unit tests can override it.
var GetKubevirtClientFromClientConfig = func(cmdConfig clientcmd.ClientConfig) (KubevirtClient, error) {
	config, err := cmdConfig.ClientConfig()
	if err != nil {
		return nil, err
	}
	return &kubevirtClient{config: config}, nil
}
