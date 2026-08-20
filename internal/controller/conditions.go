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

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"kubevirt.io/virt-import-api/core/v1alpha1"

	"kubevirt.io/virt-import/internal/logs"
)

const (
	logStatus  = "status"
	logReason  = "reason"
	logMessage = "message"
)

// shouldReconcile reports whether the import is still in flight. A
// Progressing condition explicitly set to False means the import reached a
// terminal state, so it must not be restarted on later spec changes.
func shouldReconcile(vmImport *v1alpha1.VirtualMachineImport) bool {
	progressing := meta.FindStatusCondition(vmImport.Status.Conditions, v1alpha1.ConditionProgressing)

	return progressing == nil ||
		progressing.Status != metav1.ConditionFalse
}

func setReadyCondition(
	ctx context.Context, vmImport *v1alpha1.VirtualMachineImport,
	status metav1.ConditionStatus, reason, message string, messageArgs ...any,
) {
	formattedMsg := fmt.Sprintf(message, messageArgs...)
	logf.FromContext(ctx).V(logs.TraceLevel).Info("Setting Ready condition", logStatus, status, logReason, reason, logMessage, formattedMsg)
	meta.SetStatusCondition(&vmImport.Status.Conditions, metav1.Condition{
		Type:               v1alpha1.ConditionReady,
		Status:             status,
		ObservedGeneration: vmImport.Generation,
		Reason:             reason,
		Message:            formattedMsg,
	})
}

func setProgressingCondition(
	ctx context.Context, vmImport *v1alpha1.VirtualMachineImport,
	status metav1.ConditionStatus, reason string,
) {
	const message = ""
	logf.FromContext(ctx).V(logs.TraceLevel).Info("Setting Progressing condition", logStatus, status, logReason, reason, logMessage, message)
	meta.SetStatusCondition(&vmImport.Status.Conditions, metav1.Condition{
		Type:               v1alpha1.ConditionProgressing,
		Status:             status,
		ObservedGeneration: vmImport.Generation,
		Reason:             reason,
		Message:            message,
	})
}
