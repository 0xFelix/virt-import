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

package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/selection"
	"k8s.io/client-go/discovery"
	cdiv1beta1 "kubevirt.io/containerized-data-importer-api/pkg/apis/core/v1beta1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	templatev1beta1 "kubevirt.io/virt-template-api/core/v1beta1"

	"kubevirt.io/virt-import-api/core/v1alpha1"
)

const (
	defaultPollInterval = 1 * time.Minute

	cdiGroupVersion      = "cdi.kubevirt.io/v1beta1"
	templateGroupVersion = "template.kubevirt.io/v1beta1"
)

// requiredGroups must exist before the import controller can start: it watches
// and creates CDI DataVolumes for every disk in an artifact.
//
// template.kubevirt.io is deliberately absent. It is only needed when an
// artifact's artifactType names a VirtualMachineTemplate, so it is treated as
// optional and read without a cache, see ExternalCRDCacheConfig.
var requiredGroups = []string{
	cdiGroupVersion,
}

// AvailabilityController polls for required external CRDs and starts the
// import controller once all are available. While waiting, it sets status
// conditions on pending VirtualMachineImports indicating which CRDs are
// missing.
type AvailabilityController struct {
	Manager         ctrl.Manager
	DiscoveryClient discovery.DiscoveryInterface

	pollInterval time.Duration
}

// SetPollInterval overrides the default CRD poll interval. Intended for tests.
func (c *AvailabilityController) SetPollInterval(d time.Duration) {
	c.pollInterval = d
}

func (c *AvailabilityController) Start(ctx context.Context) error {
	if len(c.missingGroups()) == 0 {
		return c.startController(ctx)
	}

	if _, err := c.Manager.GetCache().GetInformer(ctx, &v1alpha1.VirtualMachineImport{}); err != nil {
		return fmt.Errorf("failed to add VirtualMachineImport informer: %w", err)
	}

	logf.FromContext(ctx).Info("Waiting for required CRDs to become available", "groups", requiredGroups)

	if c.pollInterval == 0 {
		c.SetPollInterval(defaultPollInterval)
	}
	ticker := time.NewTicker(c.pollInterval)
	defer ticker.Stop()

	for {
		missing := c.missingGroups()
		if len(missing) == 0 {
			return c.startController(ctx)
		}

		c.setMissingCRDStatus(ctx, missing)

		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

func (c *AvailabilityController) startController(ctx context.Context) error {
	logf.FromContext(ctx).Info("All required CRDs are available, starting controller")
	return (&VirtualMachineImportReconciler{
		Client: c.Manager.GetClient(),
		Scheme: c.Manager.GetScheme(),
	}).SetupWithManager(c.Manager)
}

func (c *AvailabilityController) setMissingCRDStatus(ctx context.Context, missing []string) {
	log := logf.FromContext(ctx)
	cl := c.Manager.GetClient()

	list := &v1alpha1.VirtualMachineImportList{}
	if err := cl.List(ctx, list); err != nil {
		log.Error(err, "Failed to list VirtualMachineImports")
		return
	}

	message := fmt.Sprintf("Required CRDs are not yet available: %s", strings.Join(missing, ", "))
	for i := range list.Items {
		vmImport := &list.Items[i]

		if !vmImport.DeletionTimestamp.IsZero() || !shouldReconcile(vmImport) {
			continue
		}

		vmImportCopy := vmImport.DeepCopy()
		setReadyCondition(ctx, vmImport, metav1.ConditionFalse, v1alpha1.ReasonWaiting, "%s", message)
		setProgressingCondition(ctx, vmImport, metav1.ConditionTrue, v1alpha1.ReasonWaiting)

		if err := cl.Status().Patch(ctx, vmImport, client.MergeFrom(vmImportCopy)); err != nil {
			log.Error(err, "Failed to update VirtualMachineImport status",
				"namespace", vmImport.Namespace, "name", vmImport.Name)
		}
	}
}

func (c *AvailabilityController) missingGroups() []string {
	var missing []string
	for _, group := range requiredGroups {
		if !isAPIGroupAvailable(c.DiscoveryClient, group) {
			missing = append(missing, group)
		}
	}
	return missing
}

func isAPIGroupAvailable(dc discovery.DiscoveryInterface, groupVersion string) bool {
	_, err := dc.ServerResourcesForGroupVersion(groupVersion)
	return err == nil
}

// ExternalCRDCacheConfig builds the manager's cache configuration for CRDs that
// may not be installed. Entries are only added for groups that actually exist,
// since a cache.ByObject for a missing CRD fails manager construction.
func ExternalCRDCacheConfig(dc discovery.DiscoveryInterface) (map[client.Object]cache.ByObject, []client.Object) {
	uidReq, _ := labels.NewRequirement(v1alpha1.LabelImportUID, selection.Exists, nil)
	uidSelector := labels.NewSelector().Add(*uidReq)

	cacheByObject := map[client.Object]cache.ByObject{}
	var clientDisableFor []client.Object

	// Only cache DataVolumes this controller created, not every DataVolume in the cluster.
	if isAPIGroupAvailable(dc, cdiGroupVersion) {
		cacheByObject[&cdiv1beta1.DataVolume{}] = cache.ByObject{Label: uidSelector}
	}
	// VirtualMachineTemplates are a create-only import target. Read them
	// straight from the API server so no informer is started for a CRD that
	// may not be installed.
	if isAPIGroupAvailable(dc, templateGroupVersion) {
		clientDisableFor = append(clientDisableFor, &templatev1beta1.VirtualMachineTemplate{})
	}

	return cacheByObject, clientDisableFor
}
