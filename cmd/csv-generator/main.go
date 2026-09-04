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

// csv-generator writes a ClusterServiceVersion for virt-import to stdout.
//
// It is shipped in the controller image and invoked by the
// hyperconverged-cluster-operator, which runs the image with this binary as
// its entrypoint and consumes the manifests it prints. The install strategy
// and permissions are assembled from a rendered config/csv baked into
// the image; the surrounding metadata is embedded from csv-base.yaml.
package main

import (
	"bufio"
	"bytes"
	_ "embed"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"slices"
	"time"

	"github.com/blang/semver/v4"
	"github.com/operator-framework/api/pkg/lib/version"
	csvv1 "github.com/operator-framework/api/pkg/operators/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/yaml"
)

//go:embed csv-base.yaml
var csvBase []byte

const (
	// defaultManifestsFile is where the Dockerfile places the rendered
	// config/csv inside the controller image.
	defaultManifestsFile = "/data/manifests.yaml"

	csvNamePrefix = "virt-import.v"
	managerName   = "manager"
)

type flags struct {
	manifestsFile       string
	csvVersion          string
	replacesCsvVersion  string
	namespace           string
	operatorImage       string
	operatorVersion     string
	pullPolicy          string
	dumpCRDs            bool
	dumpNetworkPolicies bool
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}
}

func run() error {
	f := parseFlags()

	if err := f.validate(); err != nil {
		return err
	}

	m, err := readManifests(f.manifestsFile)
	if err != nil {
		return fmt.Errorf("reading %s: %w", f.manifestsFile, err)
	}

	csv, err := buildCSV(f, m)
	if err != nil {
		return err
	}

	if err := marshalObject(csv, os.Stdout); err != nil {
		return err
	}

	if f.dumpCRDs {
		if err := dumpDocuments(m.crds, os.Stdout); err != nil {
			return err
		}
	}

	if f.dumpNetworkPolicies {
		if err := dumpDocuments(m.networkPolicies, os.Stdout); err != nil {
			return err
		}
	}

	return nil
}

func parseFlags() *flags {
	f := &flags{}

	flag.StringVar(&f.manifestsFile, "manifests-file", defaultManifestsFile,
		"rendered config/csv to build the install strategy from")
	flag.StringVar(&f.csvVersion, "csv-version", "", "version of the CSV manifest (required)")
	flag.StringVar(&f.replacesCsvVersion, "replaces-csv-version", "", "CSV version this one replaces")
	flag.StringVar(&f.namespace, "namespace", "", "namespace virt-import is deployed to (required)")
	flag.StringVar(&f.operatorImage, "operator-image", "", "virt-import controller image (required)")
	flag.StringVar(&f.operatorVersion, "operator-version", "", "virt-import version")
	flag.StringVar(&f.pullPolicy, "pull-policy", string(corev1.PullIfNotPresent), "image pull policy")
	flag.BoolVar(&f.dumpCRDs, "dump-crds", false, "also dump the virt-import CRDs to stdout")
	flag.BoolVar(&f.dumpNetworkPolicies, "dump-network-policies", false,
		"also dump the virt-import network policies to stdout")
	flag.Parse()

	return f
}

func (f *flags) validate() error {
	var missing []string

	for name, value := range map[string]string{
		"csv-version":    f.csvVersion,
		"namespace":      f.namespace,
		"operator-image": f.operatorImage,
	} {
		if value == "" {
			missing = append(missing, name)
		}
	}

	if len(missing) > 0 {
		slices.Sort(missing)

		return fmt.Errorf("missing required flags: %v", missing)
	}

	if _, err := semver.Parse(f.csvVersion); err != nil {
		return fmt.Errorf("csv-version %q is not a semantic version: %w", f.csvVersion, err)
	}

	return nil
}

// manifests holds the rendered objects, bucketed by the role they play in the CSV.
type manifests struct {
	deployment          *appsv1.Deployment
	serviceAccounts     map[string]bool
	clusterRoles        map[string]*rbacv1.ClusterRole
	roles               map[string]*rbacv1.Role
	clusterRoleBindings []*rbacv1.ClusterRoleBinding
	roleBindings        []*rbacv1.RoleBinding
	crds                [][]byte
	networkPolicies     [][]byte
}

func readManifests(path string) (*manifests, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	m := &manifests{
		serviceAccounts: map[string]bool{},
		clusterRoles:    map[string]*rbacv1.ClusterRole{},
		roles:           map[string]*rbacv1.Role{},
	}

	reader := utilyaml.NewYAMLReader(bufio.NewReader(file))
	for {
		doc, err := reader.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		if len(bytes.TrimSpace(doc)) == 0 {
			continue
		}
		if err := m.add(doc); err != nil {
			return nil, err
		}
	}

	if m.deployment == nil {
		return nil, fmt.Errorf("no Deployment found in %s", path)
	}

	return m, nil
}

func (m *manifests) add(doc []byte) error {
	var typeMeta struct {
		Kind string `json:"kind"`
	}
	if err := yaml.Unmarshal(doc, &typeMeta); err != nil {
		return err
	}

	switch typeMeta.Kind {
	case "Deployment":
		return decodeInto(doc, func(o *appsv1.Deployment) { m.deployment = o })
	case "ServiceAccount":
		return decodeInto(doc, func(o *corev1.ServiceAccount) { m.serviceAccounts[o.Name] = true })
	case "ClusterRole":
		return decodeInto(doc, func(o *rbacv1.ClusterRole) { m.clusterRoles[o.Name] = o })
	case "Role":
		return decodeInto(doc, func(o *rbacv1.Role) { m.roles[o.Name] = o })
	case "ClusterRoleBinding":
		return decodeInto(doc, func(o *rbacv1.ClusterRoleBinding) {
			m.clusterRoleBindings = append(m.clusterRoleBindings, o)
		})
	case "RoleBinding":
		return decodeInto(doc, func(o *rbacv1.RoleBinding) { m.roleBindings = append(m.roleBindings, o) })
	case "CustomResourceDefinition":
		m.crds = append(m.crds, doc)
	case "NetworkPolicy":
		m.networkPolicies = append(m.networkPolicies, doc)
	}

	// Anything else a CSV cannot express is deliberately dropped.
	return nil
}

// decodeInto unmarshals doc into a fresh T and hands it to apply.
func decodeInto[T any](doc []byte, apply func(*T)) error {
	obj := new(T)
	if err := yaml.Unmarshal(doc, obj); err != nil {
		return err
	}
	apply(obj)

	return nil
}

// boundServiceAccount returns the service account a binding grants to, if it is
// one of ours. A CSV expresses permissions per service account, so bindings to
// anything else cannot be represented and are skipped.
func (m *manifests) boundServiceAccount(subjects []rbacv1.Subject) string {
	for _, subject := range subjects {
		if subject.Kind == rbacv1.ServiceAccountKind && m.serviceAccounts[subject.Name] {
			return subject.Name
		}
	}

	return ""
}

// permissionBuilder collects rules per service account, preserving the order
// they were first seen. A CSV carries one entry per service account, so rules
// from several bindings to the same account are merged rather than repeated.
type permissionBuilder struct {
	order []string
	rules map[string][]rbacv1.PolicyRule
}

func newPermissionBuilder() *permissionBuilder {
	return &permissionBuilder{rules: map[string][]rbacv1.PolicyRule{}}
}

func (b *permissionBuilder) add(serviceAccount string, rules []rbacv1.PolicyRule) {
	if _, seen := b.rules[serviceAccount]; !seen {
		b.order = append(b.order, serviceAccount)
	}
	b.rules[serviceAccount] = append(b.rules[serviceAccount], rules...)
}

func (b *permissionBuilder) build() []csvv1.StrategyDeploymentPermissions {
	var permissions []csvv1.StrategyDeploymentPermissions

	for _, serviceAccount := range b.order {
		permissions = append(permissions, csvv1.StrategyDeploymentPermissions{
			ServiceAccountName: serviceAccount,
			Rules:              b.rules[serviceAccount],
		})
	}

	return permissions
}

func (m *manifests) clusterPermissions() []csvv1.StrategyDeploymentPermissions {
	builder := newPermissionBuilder()

	for _, binding := range m.clusterRoleBindings {
		sa := m.boundServiceAccount(binding.Subjects)
		role, found := m.clusterRoles[binding.RoleRef.Name]
		if sa == "" || !found {
			continue
		}
		builder.add(sa, role.Rules)
	}

	return builder.build()
}

func (m *manifests) permissions() []csvv1.StrategyDeploymentPermissions {
	builder := newPermissionBuilder()

	for _, binding := range m.roleBindings {
		sa := m.boundServiceAccount(binding.Subjects)
		role, found := m.roles[binding.RoleRef.Name]
		if sa == "" || !found {
			continue
		}
		builder.add(sa, role.Rules)
	}

	return builder.build()
}

func buildCSV(f *flags, m *manifests) (*csvv1.ClusterServiceVersion, error) {
	csv := &csvv1.ClusterServiceVersion{}
	if err := yaml.Unmarshal(csvBase, csv); err != nil {
		return nil, fmt.Errorf("parsing embedded csv-base.yaml: %w", err)
	}

	parsed, err := semver.Parse(f.csvVersion)
	if err != nil {
		return nil, err
	}

	csv.Name = csvNamePrefix + f.csvVersion
	csv.Namespace = f.namespace
	csv.Spec.Version = version.OperatorVersion{Version: parsed}
	csv.Spec.Replaces = f.replacesCsvVersion

	if csv.Annotations == nil {
		csv.Annotations = map[string]string{}
	}
	csv.Annotations["containerImage"] = f.operatorImage
	csv.Annotations["createdAt"] = time.Now().UTC().Format(time.RFC3339)

	deployment := m.deployment.DeepCopy()
	applyImage(deployment, f.operatorImage, corev1.PullPolicy(f.pullPolicy))

	csv.Spec.InstallStrategy = csvv1.NamedInstallStrategy{
		StrategyName: csvv1.InstallStrategyNameDeployment,
		StrategySpec: csvv1.StrategyDetailsDeployment{
			DeploymentSpecs: []csvv1.StrategyDeploymentSpec{{
				Name:  deployment.Name,
				Spec:  deployment.Spec,
				Label: deployment.Labels,
			}},
			ClusterPermissions: m.clusterPermissions(),
			Permissions:        m.permissions(),
		},
	}

	csv.Spec.RelatedImages = []csvv1.RelatedImage{{
		Name:  "virt-import-controller",
		Image: f.operatorImage,
	}}

	return csv, nil
}

func applyImage(deployment *appsv1.Deployment, image string, pullPolicy corev1.PullPolicy) {
	containers := deployment.Spec.Template.Spec.Containers
	for i := range containers {
		if containers[i].Name != managerName {
			continue
		}
		containers[i].Image = image
		containers[i].ImagePullPolicy = pullPolicy
	}
}

// marshalObject writes obj as YAML, stripping the empty scaffolding that
// round-tripping through the typed structs adds.
func marshalObject(obj any, writer io.Writer) error {
	data, err := yaml.Marshal(obj)
	if err != nil {
		return err
	}

	var content map[string]any
	if unmarshalErr := yaml.Unmarshal(data, &content); unmarshalErr != nil {
		return unmarshalErr
	}

	object := unstructured.Unstructured{Object: content}
	unstructured.RemoveNestedField(object.Object, "status")
	unstructured.RemoveNestedField(object.Object, "metadata", "creationTimestamp")
	unstructured.RemoveNestedField(object.Object, "spec", "install", "spec", "deployments", "template",
		"metadata", "creationTimestamp")

	cleaned, err := yaml.Marshal(object.Object)
	if err != nil {
		return err
	}

	if _, writeErr := fmt.Fprintln(writer, "---"); writeErr != nil {
		return writeErr
	}
	_, err = writer.Write(cleaned)

	return err
}

func dumpDocuments(documents [][]byte, writer io.Writer) error {
	for _, doc := range documents {
		if _, err := fmt.Fprintln(writer, "---"); err != nil {
			return err
		}
		if _, err := writer.Write(doc); err != nil {
			return err
		}
	}

	return nil
}
