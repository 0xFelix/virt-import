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

package create

import (
	"fmt"

	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"

	importapi "kubevirt.io/virt-import-api/core"
	"kubevirt.io/virt-import-api/core/v1alpha1"
	importclient "kubevirt.io/virt-import-client-go/virtimport"

	"kubevirt.io/virt-import/internal/virtimportctl/clientconfig"
	"kubevirt.io/virt-import/internal/virtimportctl/templates"
)

// GetImportClient is used in unit tests to allow overriding the client
var GetImportClient = func(c *rest.Config) (importclient.Interface, error) {
	return importclient.NewForConfig(c)
}

const (
	urlFlag           = "url"
	targetNameFlag    = "target-name"
	secretFlag        = "secret"
	certConfigMapFlag = "cert-configmap"
	storageClassFlag  = "storage-class"
	archFlag          = "arch"
)

type create struct {
	url           string
	targetName    string
	secret        string
	certConfigMap string
	storageClass  string
	arch          string

	cmd       *cobra.Command
	namespace string
	client    importclient.Interface
}

func NewCommand() *cobra.Command {
	c := create{}
	cmd := &cobra.Command{
		Use:     "create NAME",
		Short:   "Create a VirtualMachineImport importing a VirtualMachine or VirtualMachineTemplate from an OCI artifact.",
		Example: usage(),
		Args:    cobra.ExactArgs(1),
		RunE:    c.run,
	}

	cmd.Flags().StringVar(&c.url, urlFlag, "",
		"Specify the docker:// URL of the OCI artifact, e.g. docker://registry.example.com/vms/fedora:v1.")
	cmd.Flags().StringVar(&c.targetName, targetNameFlag, "",
		"Specify the name of the created VirtualMachine or VirtualMachineTemplate. If not specified, uses NAME.")
	cmd.Flags().StringVar(&c.secret, secretFlag, "",
		"Specify the name of a Secret with the registry credentials, in the keys accessKeyId and secretKey.")
	cmd.Flags().StringVar(&c.certConfigMap, certConfigMapFlag, "",
		"Specify the name of a ConfigMap with CA certificates of the registry.")
	cmd.Flags().StringVar(&c.storageClass, storageClassFlag, "",
		"Specify the storage class of the imported disks. If not specified, uses the default storage class.")
	cmd.Flags().StringVar(&c.arch, archFlag, "",
		"Specify the architecture to import from a multi-arch artifact.")

	if err := cmd.MarkFlagRequired(urlFlag); err != nil {
		panic(err)
	}
	cmd.SetUsageTemplate(templates.UsageTemplate())

	return cmd
}

func usage() string {
	return `  # Import a VirtualMachine or VirtualMachineTemplate into the current namespace.
  {{ProgramName}} create fedora --url docker://registry.example.com/vms/fedora:v1

  # Import the arm64 variant of a multi-arch artifact under a different name.
  {{ProgramName}} create fedora --url docker://registry.example.com/vms/fedora:v1 --arch arm64 --target-name fedora-arm

  # Import from a private registry with its own CA.
  {{ProgramName}} create fedora --url docker://registry.example.com/vms/fedora:v1 --secret registry-credentials --cert-configmap registry-ca

  # Import and watch for completion.
  {{ProgramName}} create fedora --url docker://registry.example.com/vms/fedora:v1 && kubectl get vmimport fedora -w
  `
}

func (c *create) run(cmd *cobra.Command, args []string) error {
	if err := c.setDefaults(cmd); err != nil {
		return err
	}

	vmImport, err := c.createImport(args[0])
	if err != nil {
		return err
	}

	c.cmd.Printf("%s.%s/%s created\n", importapi.SingularResourceName, v1alpha1.GroupVersion.Group, vmImport.Name)

	return nil
}

func (c *create) setDefaults(cmd *cobra.Command) error {
	virtClient, namespace, _, err := clientconfig.ClientAndNamespaceFromContext(cmd.Context())
	if err != nil {
		return err
	}
	client, err := GetImportClient(virtClient.Config())
	if err != nil {
		return err
	}

	c.cmd = cmd
	c.namespace = namespace
	c.client = client

	return nil
}

func (c *create) createImport(name string) (*v1alpha1.VirtualMachineImport, error) {
	registry := &v1alpha1.VirtualMachineImportRegistrySource{
		URL:           c.url,
		SecretRef:     c.secret,
		CertConfigMap: c.certConfigMap,
	}
	if c.arch != "" {
		registry.Platform = &v1alpha1.PlatformOptions{Architecture: c.arch}
	}

	vmImport := &v1alpha1.VirtualMachineImport{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: c.namespace,
		},
		Spec: v1alpha1.VirtualMachineImportSpec{
			Source:           v1alpha1.VirtualMachineImportSource{Registry: registry},
			TargetName:       c.targetName,
			StorageClassName: c.storageClass,
		},
	}

	vmImport, err := c.client.ImportV1alpha1().VirtualMachineImports(c.namespace).
		Create(c.cmd.Context(), vmImport, metav1.CreateOptions{})
	if err != nil {
		return nil, fmt.Errorf("error creating VirtualMachineImport: %w", err)
	}

	return vmImport, nil
}
