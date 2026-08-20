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

	"k8s.io/apimachinery/pkg/runtime"
	cdiv1beta1 "kubevirt.io/containerized-data-importer-api/pkg/apis/core/v1beta1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	importapi "kubevirt.io/virt-import-api/core"
	"kubevirt.io/virt-import-api/core/v1alpha1"
)

// VirtualMachineImportReconciler reconciles a VirtualMachineImport object.
type VirtualMachineImportReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=import.kubevirt.io,resources=virtualmachineimports,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=import.kubevirt.io,resources=virtualmachineimports/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=import.kubevirt.io,resources=virtualmachineimports/finalizers,verbs=update
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=pods/log,verbs=get
// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=cdi.kubevirt.io,resources=datavolumes,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups=kubevirt.io,resources=virtualmachines,verbs=get;list;watch;create
// +kubebuilder:rbac:groups=template.kubevirt.io,resources=virtualmachinetemplates,verbs=get;list;watch;create

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
//
// The import state machine is not implemented yet, see VEP #395.
func (r *VirtualMachineImportReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	vmImport := &v1alpha1.VirtualMachineImport{}
	if err := r.Get(ctx, req.NamespacedName, vmImport); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *VirtualMachineImportReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.VirtualMachineImport{}).
		Owns(&cdiv1beta1.DataVolume{}).
		Named(importapi.SingularResourceName).
		Complete(r)
}
