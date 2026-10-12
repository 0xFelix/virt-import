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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/distribution/reference"
	"github.com/opencontainers/go-digest"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/cluster-api/util/patch"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	cdiv1beta1 "kubevirt.io/containerized-data-importer-api/pkg/apis/core/v1beta1"

	importapi "kubevirt.io/virt-import-api/core"
	"kubevirt.io/virt-import-api/core/v1alpha1"

	"kubevirt.io/virt-import/internal/apimachinery"
	"kubevirt.io/virt-import/internal/artifact"
	"kubevirt.io/virt-import/internal/logs"
)

const (
	uidField = "metadata.uid"

	dockerScheme = "docker://"

	// FetcherContainerName is the name of the container in the metadata-fetch Pod.
	FetcherContainerName = "fetcher"
	fetchJobSuffix       = "fetch"
	fetchJobBackoffLimit = 3
	fetchJobDeadline     = 10 * time.Minute
	certsVolumeName      = "registry-certs"
	certsMountPath       = "/etc/virt-import/certs"
	tmpVolumeName        = "tmp"
	tmpMountPath         = "/tmp"

	// Environment variables the fetcher reads the registry credentials from, and the keys of
	// the Secret they come from. The keys are the ones CDI uses for DataVolume registry sources.
	envAccessKeyID = "ACCESS_KEY_ID"
	envSecretKey   = "SECRET_KEY"
	keyAccessKeyID = "accessKeyId"
	keySecretKey   = "secretKey"

	manifestSuffix = "manifest"
	// ManifestKeyTarget holds the VirtualMachine or VirtualMachineTemplate to create.
	ManifestKeyTarget = "target"
	// ManifestKeyResult holds the artifact.FetchResult, without the config blob.
	ManifestKeyResult = "result"

	cdiConfigName       = "config"
	annImmediateBinding = "cdi.kubevirt.io/storage.bind.immediate.requested"

	logNS        = "ns"
	logName      = "name"
	logJobName   = "jobName"
	logDVName    = "dvName"
	logKind      = "kind"
	logStatus    = "status"
	logReason    = "reason"
	logMessage   = "message"
	logUID       = "uid"
	logCount     = "count"
	logURL       = "url"
	logPodName   = "podName"
	logPVCName   = "pvcName"
	logFinalizer = "finalizer"
)

// Container states of the fetcher that need the user's attention, e.g. a missing Secret.
var fetcherWaitingReasons = []string{
	"CreateContainerConfigError",
	"CreateContainerError",
	"ErrImagePull",
	"ImagePullBackOff",
	"InvalidImageName",
}

// VirtualMachineImportReconciler reconciles a VirtualMachineImport object.
type VirtualMachineImportReconciler struct {
	client.Client
	Scheme  *runtime.Scheme
	PodLogs PodLogReader
	// FetcherImage is the image of the metadata-fetch Job.
	FetcherImage string
}

// +kubebuilder:rbac:groups=import.kubevirt.io,resources=virtualmachineimports,verbs=get;list;watch;patch
// +kubebuilder:rbac:groups=import.kubevirt.io,resources=virtualmachineimports/status,verbs=get;patch
// +kubebuilder:rbac:groups=import.kubevirt.io,resources=virtualmachineimports/finalizers,verbs=update
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=pods/log,verbs=get
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;patch
// +kubebuilder:rbac:groups=cdi.kubevirt.io,resources=datavolumes,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups=cdi.kubevirt.io,resources=cdiconfigs,verbs=get;list;watch
// +kubebuilder:rbac:groups=kubevirt.io,resources=virtualmachines,verbs=get;create
// +kubebuilder:rbac:groups=template.kubevirt.io,resources=virtualmachinetemplates,verbs=get;create

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
//
// An import fetches the artifact metadata with a Job, persists the target to create in a
// ConfigMap, imports every disk with a DataVolume and finally creates the target.
func (r *VirtualMachineImportReconciler) Reconcile(ctx context.Context, req ctrl.Request) (result ctrl.Result, retErr error) {
	log := logf.FromContext(ctx)

	vmImport := &v1alpha1.VirtualMachineImport{}
	if err := r.Get(ctx, req.NamespacedName, vmImport); err != nil {
		if !k8serrors.IsNotFound(err) {
			log.Error(err, "Unable to fetch VirtualMachineImport")
		}
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !vmImport.DeletionTimestamp.IsZero() {
		log.Info("Handling deletion of VirtualMachineImport")
		return ctrl.Result{}, r.handleDeletion(ctx, vmImport)
	}
	if err := r.addFinalizer(ctx, vmImport); err != nil {
		return ctrl.Result{}, err
	}

	if !shouldReconcile(vmImport) {
		log.V(logs.DebugLevel).Info("VirtualMachineImport is no longer progressing, not reconciling")
		return ctrl.Result{}, r.handleFinished(ctx, vmImport)
	}

	helper, err := patch.NewHelper(vmImport, r.Client)
	if err != nil {
		return ctrl.Result{}, err
	}
	// Remove Progressing condition to reset state for this reconcile.
	// It is set by the step the import is in, or by setStatusConditions.
	step := currentStep(vmImport)
	meta.RemoveStatusCondition(&vmImport.Status.Conditions, v1alpha1.ConditionProgressing)
	defer func() {
		setStatusConditions(ctx, vmImport, step, retErr)
		retErr = errors.Join(retErr, helper.Patch(ctx, vmImport))
	}()

	if validateErr := validateImport(vmImport); validateErr != nil {
		log.Error(validateErr, "Error validating VirtualMachineImport")
		return ctrl.Result{}, failImport(ctx, vmImport, validateErr)
	}

	cm, manifestResult, err := r.reconcileManifest(ctx, vmImport)
	if manifestResult != nil || err != nil {
		return ptr.Deref(manifestResult, ctrl.Result{}), err
	}
	fetchResult, target, err := decodeManifest(cm)
	if err != nil {
		return ctrl.Result{}, failImport(ctx, vmImport, err)
	}
	setArtifactInfo(vmImport, fetchResult)

	if ready, dvErr := r.reconcileDataVolumes(ctx, vmImport, fetchResult); !ready || dvErr != nil {
		return ctrl.Result{}, dvErr
	}

	return ctrl.Result{}, r.reconcileTarget(ctx, vmImport, target, len(fetchResult.Layers))
}

func (r *VirtualMachineImportReconciler) addFinalizer(
	ctx context.Context, vmImport *v1alpha1.VirtualMachineImport,
) error {
	if !controllerutil.ContainsFinalizer(vmImport, v1alpha1.FinalizerImportCleanup) {
		logf.FromContext(ctx).V(logs.TraceLevel).Info("Adding finalizer to VirtualMachineImport")
		vmImportCopy := vmImport.DeepCopy()
		controllerutil.AddFinalizer(vmImport, v1alpha1.FinalizerImportCleanup)
		if err := r.Patch(ctx, vmImport, client.MergeFrom(vmImportCopy)); err != nil {
			return err
		}
	}

	return nil
}

func (r *VirtualMachineImportReconciler) removeFinalizer(
	ctx context.Context, vmImport *v1alpha1.VirtualMachineImport,
) error {
	logf.FromContext(ctx).V(logs.TraceLevel).Info("Removing finalizer from VirtualMachineImport",
		logFinalizer, v1alpha1.FinalizerImportCleanup)
	vmImportCopy := vmImport.DeepCopy()
	controllerutil.RemoveFinalizer(vmImport, v1alpha1.FinalizerImportCleanup)
	return r.Patch(ctx, vmImport, client.MergeFrom(vmImportCopy))
}

// handleDeletion cleans up before the VirtualMachineImport goes away. An import that completed
// keeps its PVCs, any other import loses everything it created.
func (r *VirtualMachineImportReconciler) handleDeletion(
	ctx context.Context, vmImport *v1alpha1.VirtualMachineImport,
) error {
	if !controllerutil.ContainsFinalizer(vmImport, v1alpha1.FinalizerImportCleanup) {
		return nil
	}

	logf.FromContext(ctx).V(logs.TraceLevel).Info("Finalizing VirtualMachineImport")
	if isReady(vmImport) {
		if err := r.cleanupAfterSuccess(ctx, vmImport); err != nil {
			return err
		}
	} else if err := r.deleteImportResources(ctx, vmImport); err != nil {
		return err
	}

	return r.removeFinalizer(ctx, vmImport)
}

// handleFinished hands the PVCs of a completed import over to its target.
func (r *VirtualMachineImportReconciler) handleFinished(
	ctx context.Context, vmImport *v1alpha1.VirtualMachineImport,
) error {
	if !isReady(vmImport) || !controllerutil.ContainsFinalizer(vmImport, v1alpha1.FinalizerImportCleanup) {
		return nil
	}

	if err := r.cleanupAfterSuccess(ctx, vmImport); err != nil {
		return err
	}
	return r.removeFinalizer(ctx, vmImport)
}

// cleanupAfterSuccess removes the DataVolume owner references from the imported PVCs and deletes
// the DataVolumes, so the PVCs persist independently of the import.
func (r *VirtualMachineImportReconciler) cleanupAfterSuccess(
	ctx context.Context, vmImport *v1alpha1.VirtualMachineImport,
) error {
	log := logf.FromContext(ctx)

	if err := r.deleteFetchJob(ctx, vmImport); err != nil {
		return err
	}

	dvs, err := r.listDataVolumes(ctx, vmImport)
	if err != nil {
		return err
	}
	for i := range dvs {
		dv := &dvs[i]
		if err := r.orphanPVC(ctx, dv); err != nil {
			log.Error(err, "Unable to remove the DataVolume owner reference from PVC", logNS, dv.Namespace, logPVCName, dv.Name)
			return err
		}
		log.Info("Deleting DataVolume", logNS, dv.Namespace, logDVName, dv.Name)
		if err := client.IgnoreNotFound(r.Delete(ctx, dv)); err != nil {
			return err
		}
	}

	return nil
}

// deleteImportResources deletes the Job and the DataVolumes of an import, and with them their
// Pods and PVCs.
func (r *VirtualMachineImportReconciler) deleteImportResources(
	ctx context.Context, vmImport *v1alpha1.VirtualMachineImport,
) error {
	if err := r.deleteFetchJob(ctx, vmImport); err != nil {
		return err
	}

	dvs, err := r.listDataVolumes(ctx, vmImport)
	if err != nil {
		return err
	}
	for i := range dvs {
		logf.FromContext(ctx).Info("Deleting DataVolume", logNS, dvs[i].Namespace, logDVName, dvs[i].Name)
		if err := client.IgnoreNotFound(r.Delete(ctx, &dvs[i])); err != nil {
			return err
		}
	}

	return nil
}

func (r *VirtualMachineImportReconciler) orphanPVC(ctx context.Context, dv *cdiv1beta1.DataVolume) error {
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Namespace: dv.Namespace, Name: dv.Name},
	}
	if err := r.Get(ctx, client.ObjectKeyFromObject(pvc), pvc); err != nil {
		return client.IgnoreNotFound(err)
	}

	pvcCopy := pvc.DeepCopy()
	pvc.OwnerReferences = slices.DeleteFunc(pvc.OwnerReferences, func(ref metav1.OwnerReference) bool {
		return ref.UID == dv.UID
	})
	if len(pvc.OwnerReferences) == len(pvcCopy.OwnerReferences) {
		return nil
	}

	logf.FromContext(ctx).V(logs.DebugLevel).Info("Removing DataVolume owner reference from PVC",
		logNS, pvc.Namespace, logPVCName, pvc.Name)
	return r.Patch(ctx, pvc, client.MergeFrom(pvcCopy))
}

func (r *VirtualMachineImportReconciler) listDataVolumes(
	ctx context.Context, vmImport *v1alpha1.VirtualMachineImport,
) ([]cdiv1beta1.DataVolume, error) {
	list := &cdiv1beta1.DataVolumeList{}
	if err := r.List(ctx, list, client.InNamespace(vmImport.Namespace),
		client.MatchingLabels{v1alpha1.LabelImportUID: string(vmImport.UID)}); err != nil {
		return nil, err
	}
	return list.Items, nil
}

func (r *VirtualMachineImportReconciler) deleteFetchJob(
	ctx context.Context, vmImport *v1alpha1.VirtualMachineImport,
) error {
	job := emptyFetchJob(vmImport)
	err := r.Delete(ctx, job, client.PropagationPolicy(metav1.DeletePropagationBackground))
	if err == nil {
		logf.FromContext(ctx).V(logs.DebugLevel).Info("Deleted metadata-fetch Job", logJobName, job.Name)
	}
	return client.IgnoreNotFound(err)
}

// reconcileManifest returns the manifest ConfigMap of the import. Until it exists, it runs the
// metadata-fetch Job and creates the ConfigMap from the Job's result.
func (r *VirtualMachineImportReconciler) reconcileManifest(
	ctx context.Context, vmImport *v1alpha1.VirtualMachineImport,
) (*corev1.ConfigMap, *ctrl.Result, error) {
	cm := emptyManifestConfigMap(vmImport)
	if err := r.Get(ctx, client.ObjectKeyFromObject(cm), cm); err == nil {
		if cm.Labels[v1alpha1.LabelImportUID] != string(vmImport.UID) {
			return nil, nil, failImport(ctx, vmImport,
				fmt.Errorf("configMap %s/%s does not belong to this import", cm.Namespace, cm.Name))
		}
		// The Job is deleted right after the ConfigMap is created, unless the controller restarted in between.
		return cm, nil, r.deleteFetchJob(ctx, vmImport)
	} else if !k8serrors.IsNotFound(err) {
		return nil, nil, err
	}

	setProgressingCondition(ctx, vmImport, metav1.ConditionTrue, v1alpha1.ReasonFetchingMetadata)

	job := emptyFetchJob(vmImport)
	if err := r.Get(ctx, client.ObjectKeyFromObject(job), job); k8serrors.IsNotFound(err) {
		if createErr := r.createFetchJob(ctx, vmImport); createErr != nil {
			return nil, nil, createErr
		}
		setReadyCondition(ctx, vmImport, metav1.ConditionFalse, v1alpha1.ReasonFetchingMetadata,
			"Fetching artifact metadata from %s", vmImport.Spec.Source.Registry.URL)
		return nil, &ctrl.Result{}, nil
	} else if err != nil {
		return nil, nil, err
	}
	if job.Labels[v1alpha1.LabelImportUID] != string(vmImport.UID) {
		return nil, nil, failImport(ctx, vmImport,
			fmt.Errorf("job %s/%s does not belong to this import", job.Namespace, job.Name))
	}

	pods, err := r.listFetchPods(ctx, vmImport)
	if err != nil {
		return nil, nil, err
	}
	if failed, message := isJobConditionTrue(job, batchv1.JobFailed); failed {
		return nil, nil, failImport(ctx, vmImport,
			fmt.Errorf("fetching artifact metadata failed: %s", fetchFailureMessage(pods, message)))
	}
	if complete, _ := isJobConditionTrue(job, batchv1.JobComplete); !complete {
		syncFetchPodStatus(ctx, vmImport, pods)
		return nil, &ctrl.Result{}, nil
	}

	if cm, err = r.createManifestConfigMap(ctx, vmImport, pods); err != nil {
		return nil, nil, err
	}
	return cm, nil, r.deleteFetchJob(ctx, vmImport)
}

func (r *VirtualMachineImportReconciler) createFetchJob(
	ctx context.Context, vmImport *v1alpha1.VirtualMachineImport,
) error {
	insecure, err := r.isInsecureRegistry(ctx, vmImport.Spec.Source.Registry.URL)
	if err != nil {
		return err
	}

	job := newFetchJob(vmImport, r.FetcherImage, insecure)
	if err := ctrl.SetControllerReference(vmImport, job, r.Scheme); err != nil {
		return err
	}
	logf.FromContext(ctx).Info("Creating metadata-fetch Job", logNS, job.Namespace, logJobName, job.Name,
		logURL, vmImport.Spec.Source.Registry.URL)
	return r.Create(ctx, job)
}

// isInsecureRegistry reports whether CDI treats the registry of url as insecure. The fetcher
// follows CDI, so the metadata and the disks are read the same way.
func (r *VirtualMachineImportReconciler) isInsecureRegistry(ctx context.Context, registryURL string) (bool, error) {
	cdiConfig := &cdiv1beta1.CDIConfig{}
	if err := r.Get(ctx, client.ObjectKey{Name: cdiConfigName}, cdiConfig); err != nil {
		return false, client.IgnoreNotFound(err)
	}

	parsed, err := url.Parse(registryURL)
	if err != nil {
		return false, err
	}
	return slices.Contains(cdiConfig.Spec.InsecureRegistries, parsed.Host), nil
}

func (r *VirtualMachineImportReconciler) listFetchPods(
	ctx context.Context, vmImport *v1alpha1.VirtualMachineImport,
) ([]corev1.Pod, error) {
	list := &corev1.PodList{}
	if err := r.List(ctx, list, client.InNamespace(vmImport.Namespace),
		client.MatchingLabels{v1alpha1.LabelImportUID: string(vmImport.UID)}); err != nil {
		return nil, err
	}
	return list.Items, nil
}

// createManifestConfigMap reads the fetch result from the logs of the succeeded fetch Pod and
// persists the target built from it, so the import survives the Job and controller restarts.
func (r *VirtualMachineImportReconciler) createManifestConfigMap(
	ctx context.Context, vmImport *v1alpha1.VirtualMachineImport, pods []corev1.Pod,
) (*corev1.ConfigMap, error) {
	pod := findPod(pods, corev1.PodSucceeded)
	if pod == nil {
		return nil, fmt.Errorf("no succeeded Pod of metadata-fetch Job %s found", emptyFetchJob(vmImport).Name)
	}
	podLogs, err := r.PodLogs.GetLogs(ctx, pod.Namespace, pod.Name, FetcherContainerName)
	if err != nil {
		return nil, fmt.Errorf("failed to read the logs of Pod %s/%s: %w", pod.Namespace, pod.Name, err)
	}
	fetchResult, err := artifact.ParseResult(bytes.NewReader(podLogs))
	if err != nil {
		return nil, failImport(ctx, vmImport, fmt.Errorf("pod %s/%s: %w", pod.Namespace, pod.Name, err))
	}

	target, err := artifact.Rewrite(fetchResult.ArtifactType, fetchResult.Config, fetchResult.DiskNames(),
		&artifact.TargetOptions{
			Name:      getTargetName(vmImport),
			Namespace: vmImport.Namespace,
			ImportUID: string(vmImport.UID),
			ClaimName: func(disk string) string { return getDataVolumeName(vmImport, disk) },
		})
	if err != nil {
		return nil, failImport(ctx, vmImport, err)
	}
	err = r.checkTargetAvailable(ctx, vmImport, target)
	if err != nil {
		return nil, err
	}

	cm, err := newManifestConfigMap(vmImport, fetchResult, target)
	if err != nil {
		return nil, failImport(ctx, vmImport, err)
	}
	err = ctrl.SetControllerReference(vmImport, cm, r.Scheme)
	if err != nil {
		return nil, err
	}
	logf.FromContext(ctx).Info("Creating manifest ConfigMap", logNS, cm.Namespace, logName, cm.Name)
	err = r.Create(ctx, cm)
	if err != nil {
		return nil, err
	}

	return cm, nil
}

// checkTargetAvailable fails the import if its target already exists and was not created by it,
// or if the API serving the target is missing.
func (r *VirtualMachineImportReconciler) checkTargetAvailable(
	ctx context.Context, vmImport *v1alpha1.VirtualMachineImport, target *unstructured.Unstructured,
) error {
	existing := &unstructured.Unstructured{}
	existing.SetGroupVersionKind(target.GroupVersionKind())
	err := r.Get(ctx, client.ObjectKeyFromObject(target), existing)
	switch {
	case k8serrors.IsNotFound(err):
		return nil
	case meta.IsNoMatchError(err):
		return failImport(ctx, vmImport, fmt.Errorf("API group %s is not available", target.GroupVersionKind().Group))
	case err != nil:
		return err
	case existing.GetLabels()[v1alpha1.LabelImportUID] != string(vmImport.UID):
		return failImport(ctx, vmImport, fmt.Errorf("%s %s/%s already exists", target.GetKind(), target.GetNamespace(), target.GetName()))
	default:
		return nil
	}
}

// reconcileDataVolumes creates a DataVolume per disk and reports whether all of them succeeded.
func (r *VirtualMachineImportReconciler) reconcileDataVolumes(
	ctx context.Context, vmImport *v1alpha1.VirtualMachineImport, fetchResult *artifact.FetchResult,
) (bool, error) {
	setProgressingCondition(ctx, vmImport, metav1.ConditionTrue, v1alpha1.ReasonImportingDisks)

	diskImports := make([]v1alpha1.DiskImportStatus, 0, len(fetchResult.Layers))
	var waitingFor *cdiv1beta1.DataVolume
	for _, layer := range fetchResult.Layers {
		dv, err := r.ensureDataVolume(ctx, vmImport, fetchResult, &layer)
		if err != nil {
			return false, err
		}
		diskImports = append(diskImports, v1alpha1.DiskImportStatus{
			Name:           layer.DiskName,
			DataVolumeName: dv.Name,
			Phase:          string(dv.Status.Phase),
		})
		if dv.Status.Phase == cdiv1beta1.Succeeded {
			continue
		}
		if failed, message := isDataVolumeFailed(dv); failed {
			vmImport.Status.DiskImports = diskImports
			return false, failImport(ctx, vmImport, fmt.Errorf("dataVolume %s/%s failed: %s", dv.Namespace, dv.Name, message))
		}
		if waitingFor == nil {
			waitingFor = dv
		}
	}
	vmImport.Status.DiskImports = diskImports

	if waitingFor != nil {
		setReadyCondition(ctx, vmImport, metav1.ConditionFalse, v1alpha1.ReasonImportingDisks,
			"Waiting for DataVolume %s/%s to be ready", waitingFor.Namespace, waitingFor.Name)
		return false, nil
	}
	return true, nil
}

func (r *VirtualMachineImportReconciler) ensureDataVolume(
	ctx context.Context, vmImport *v1alpha1.VirtualMachineImport,
	fetchResult *artifact.FetchResult, layer *artifact.Layer,
) (*cdiv1beta1.DataVolume, error) {
	dv := emptyDataVolume(vmImport.Namespace, getDataVolumeName(vmImport, layer.DiskName))
	if err := r.Get(ctx, client.ObjectKeyFromObject(dv), dv); err == nil {
		if dv.Labels[v1alpha1.LabelImportUID] != string(vmImport.UID) {
			return nil, failImport(ctx, vmImport,
				fmt.Errorf("dataVolume %s/%s does not belong to this import", dv.Namespace, dv.Name))
		}
		return dv, nil
	} else if !k8serrors.IsNotFound(err) {
		return nil, err
	}

	dv, err := newDataVolume(vmImport, fetchResult, layer)
	if err != nil {
		return nil, failImport(ctx, vmImport, err)
	}
	err = ctrl.SetControllerReference(vmImport, dv, r.Scheme)
	if err != nil {
		return nil, err
	}
	logf.FromContext(ctx).Info("Creating DataVolume", logNS, dv.Namespace, logDVName, dv.Name)
	err = r.Create(ctx, dv)
	if err != nil {
		return nil, err
	}
	// An API server without the CDI layer selection API drops the selector silently, and CDI
	// would import the whole artifact as a container disk.
	if dv.Spec.Source == nil || dv.Spec.Source.Registry == nil || dv.Spec.Source.Registry.Layer == nil {
		return nil, failImport(ctx, vmImport,
			errors.New("the installed CDI does not support layer selection for DataVolume registry sources"))
	}

	return dv, nil
}

// reconcileTarget creates the VirtualMachine or VirtualMachineTemplate and completes the import.
func (r *VirtualMachineImportReconciler) reconcileTarget(
	ctx context.Context, vmImport *v1alpha1.VirtualMachineImport,
	target *unstructured.Unstructured, diskCount int,
) error {
	setProgressingCondition(ctx, vmImport, metav1.ConditionTrue, v1alpha1.ReasonCreatingTarget)

	if err := r.checkTargetAvailable(ctx, vmImport, target); err != nil {
		return err
	}
	logf.FromContext(ctx).Info("Creating import target", logKind, target.GetKind(), logNS, target.GetNamespace(),
		logName, target.GetName())
	if err := r.Create(ctx, target); err != nil && !k8serrors.IsAlreadyExists(err) {
		if meta.IsNoMatchError(err) {
			return failImport(ctx, vmImport, fmt.Errorf("API group %s is not available", target.GroupVersionKind().Group))
		}
		return err
	}

	setTargetRef(vmImport, target)
	setReadyCondition(ctx, vmImport, metav1.ConditionTrue, v1alpha1.ReasonImportComplete,
		"%s %s created with %d disk(s)", target.GetKind(), target.GetName(), diskCount)
	setProgressingCondition(ctx, vmImport, metav1.ConditionFalse, v1alpha1.ReasonImportComplete)

	return nil
}

// shouldReconcile reports whether the import is still in flight. A
// Progressing condition explicitly set to False means the import reached a
// terminal state, so it must not be restarted on later spec changes.
func shouldReconcile(vmImport *v1alpha1.VirtualMachineImport) bool {
	progressing := meta.FindStatusCondition(vmImport.Status.Conditions, v1alpha1.ConditionProgressing)

	return progressing == nil ||
		progressing.Status != metav1.ConditionFalse
}

func isReady(vmImport *v1alpha1.VirtualMachineImport) bool {
	return meta.IsStatusConditionTrue(vmImport.Status.Conditions, v1alpha1.ConditionReady)
}

func validateImport(vmImport *v1alpha1.VirtualMachineImport) error {
	if vmImport.Spec.Source.Registry == nil {
		return errors.New("spec.source.registry is required")
	}
	if _, err := parseRegistryURL(vmImport.Spec.Source.Registry.URL); err != nil {
		return err
	}

	return nil
}

// failImport marks the import as permanently failed. The returned error ends up in the Ready
// condition through setStatusConditions.
func failImport(ctx context.Context, vmImport *v1alpha1.VirtualMachineImport, err error) error {
	setProgressingCondition(ctx, vmImport, metav1.ConditionFalse, v1alpha1.ReasonImportFailed)
	return err
}

// currentStep returns the reason of the step the import was in after the previous reconcile.
func currentStep(vmImport *v1alpha1.VirtualMachineImport) string {
	progressing := meta.FindStatusCondition(vmImport.Status.Conditions, v1alpha1.ConditionProgressing)
	if progressing == nil || progressing.Status != metav1.ConditionTrue || progressing.Reason == v1alpha1.ReasonWaiting {
		return v1alpha1.ReasonFetchingMetadata
	}
	return progressing.Reason
}

// setStatusConditions fills the conditions a reconcile left unset. An error keeps the import in
// its current step and is reported in the Ready condition; after failImport it is terminal.
func setStatusConditions(ctx context.Context, vmImport *v1alpha1.VirtualMachineImport, step string, retErr error) {
	progressing := meta.FindStatusCondition(vmImport.Status.Conditions, v1alpha1.ConditionProgressing)
	if progressing == nil {
		setProgressingCondition(ctx, vmImport, metav1.ConditionTrue, step)
		progressing = meta.FindStatusCondition(vmImport.Status.Conditions, v1alpha1.ConditionProgressing)
	}

	if retErr != nil {
		logf.FromContext(ctx).Error(retErr, "Reconciliation failed")
		setReadyCondition(ctx, vmImport, metav1.ConditionFalse, progressing.Reason, "%s", retErr.Error())
		return
	}
	if meta.FindStatusCondition(vmImport.Status.Conditions, v1alpha1.ConditionReady) == nil {
		setReadyCondition(ctx, vmImport, metav1.ConditionFalse, progressing.Reason, "")
	}
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

func setArtifactInfo(vmImport *v1alpha1.VirtualMachineImport, fetchResult *artifact.FetchResult) {
	vmImport.Status.ArtifactInfo = &v1alpha1.ArtifactInfo{
		ArtifactType: fetchResult.ArtifactType,
		Architecture: fetchResult.Architecture,
		DiskCount:    len(fetchResult.Layers),
	}
}

func setTargetRef(vmImport *v1alpha1.VirtualMachineImport, target *unstructured.Unstructured) {
	vmImport.Status.TargetRef = &corev1.ObjectReference{
		APIVersion: target.GetAPIVersion(),
		Kind:       target.GetKind(),
		Namespace:  target.GetNamespace(),
		Name:       target.GetName(),
	}
}

// syncFetchPodStatus reports the progress of the metadata-fetch Pods in the Ready condition.
func syncFetchPodStatus(ctx context.Context, vmImport *v1alpha1.VirtualMachineImport, pods []corev1.Pod) {
	for i := range pods {
		for _, status := range pods[i].Status.ContainerStatuses {
			if status.State.Waiting != nil && slices.Contains(fetcherWaitingReasons, status.State.Waiting.Reason) {
				setReadyCondition(ctx, vmImport, metav1.ConditionFalse, v1alpha1.ReasonFetchingMetadata,
					"Metadata-fetch Pod %s/%s cannot start: %s: %s", pods[i].Namespace, pods[i].Name,
					status.State.Waiting.Reason, status.State.Waiting.Message)
				return
			}
		}
	}

	if message := terminationMessage(pods); message != "" {
		setReadyCondition(ctx, vmImport, metav1.ConditionFalse, v1alpha1.ReasonFetchingMetadata,
			"Fetching artifact metadata from %s, retrying after: %s", vmImport.Spec.Source.Registry.URL, message)
		return
	}
	setReadyCondition(ctx, vmImport, metav1.ConditionFalse, v1alpha1.ReasonFetchingMetadata,
		"Fetching artifact metadata from %s", vmImport.Spec.Source.Registry.URL)
}

// fetchFailureMessage returns why the metadata-fetch Job failed: the termination message of the
// fetcher, or the reason the Job gives.
func fetchFailureMessage(pods []corev1.Pod, jobMessage string) string {
	if message := terminationMessage(pods); message != "" {
		return message
	}
	return jobMessage
}

// terminationMessage returns the termination message of the most recently failed fetcher.
func terminationMessage(pods []corev1.Pod) string {
	var latest *corev1.ContainerStateTerminated
	for i := range pods {
		for _, status := range pods[i].Status.ContainerStatuses {
			terminated := status.State.Terminated
			if status.Name != FetcherContainerName || terminated == nil || terminated.ExitCode == 0 {
				continue
			}
			if latest == nil || latest.FinishedAt.Before(&terminated.FinishedAt) {
				latest = terminated
			}
		}
	}
	if latest == nil {
		return ""
	}
	return strings.TrimSpace(latest.Message)
}

func findPod(pods []corev1.Pod, phase corev1.PodPhase) *corev1.Pod {
	for i := range pods {
		if pods[i].Status.Phase == phase {
			return &pods[i]
		}
	}
	return nil
}

func isJobConditionTrue(job *batchv1.Job, condType batchv1.JobConditionType) (isTrue bool, message string) {
	for _, cond := range job.Status.Conditions {
		if cond.Type == condType && cond.Status == corev1.ConditionTrue {
			return true, cond.Message
		}
	}
	return false, ""
}

// isDataVolumeFailed reports whether a DataVolume failed, the way virt-template judges it.
func isDataVolumeFailed(dv *cdiv1beta1.DataVolume) (failed bool, message string) {
	for _, cond := range dv.Status.Conditions {
		if cond.Type != cdiv1beta1.DataVolumeRunning {
			continue
		}
		return cond.Status != corev1.ConditionTrue && dv.Status.Phase == cdiv1beta1.Failed, cond.Message
	}
	return false, ""
}

func decodeManifest(cm *corev1.ConfigMap) (*artifact.FetchResult, *unstructured.Unstructured, error) {
	fetchResult := &artifact.FetchResult{}
	if err := json.Unmarshal([]byte(cm.Data[ManifestKeyResult]), fetchResult); err != nil {
		return nil, nil, fmt.Errorf("configMap %s/%s holds an invalid %s: %w", cm.Namespace, cm.Name, ManifestKeyResult, err)
	}
	target := &unstructured.Unstructured{}
	if err := target.UnmarshalJSON([]byte(cm.Data[ManifestKeyTarget])); err != nil {
		return nil, nil, fmt.Errorf("configMap %s/%s holds an invalid %s: %w", cm.Namespace, cm.Name, ManifestKeyTarget, err)
	}
	return fetchResult, target, nil
}

func newFetchJob(vmImport *v1alpha1.VirtualMachineImport, image string, insecure bool) *batchv1.Job {
	registry := vmImport.Spec.Source.Registry
	labels := map[string]string{v1alpha1.LabelImportUID: string(vmImport.UID)}

	args := []string{"--url=" + registry.URL}
	if registry.Platform != nil && ptr.Deref(registry.Platform.Architecture, "") != "" {
		args = append(args, "--arch="+*registry.Platform.Architecture)
	}
	if insecure {
		args = append(args, "--insecure-tls")
	}

	container := corev1.Container{
		Name:  FetcherContainerName,
		Image: image,
		Args:  args,
		VolumeMounts: []corev1.VolumeMount{
			{Name: tmpVolumeName, MountPath: tmpMountPath},
		},
		TerminationMessagePolicy: corev1.TerminationMessageReadFile,
		SecurityContext: &corev1.SecurityContext{
			AllowPrivilegeEscalation: ptr.To(false),
			Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
			ReadOnlyRootFilesystem:   ptr.To(true),
		},
	}
	volumes := []corev1.Volume{
		{Name: tmpVolumeName, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
	}
	if registry.SecretRef != nil && *registry.SecretRef != "" {
		container.Env = []corev1.EnvVar{
			secretEnvVar(envAccessKeyID, *registry.SecretRef, keyAccessKeyID),
			secretEnvVar(envSecretKey, *registry.SecretRef, keySecretKey),
		}
	}
	if registry.CertConfigMap != nil && *registry.CertConfigMap != "" {
		container.Args = append(container.Args, "--cert-dir="+certsMountPath)
		container.VolumeMounts = append(container.VolumeMounts,
			corev1.VolumeMount{Name: certsVolumeName, MountPath: certsMountPath, ReadOnly: true})
		volumes = append(volumes, corev1.Volume{
			Name: certsVolumeName,
			VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: *registry.CertConfigMap},
			}},
		})
	}

	job := emptyFetchJob(vmImport)
	job.Labels = labels
	job.Spec = batchv1.JobSpec{
		BackoffLimit:          ptr.To[int32](fetchJobBackoffLimit),
		ActiveDeadlineSeconds: ptr.To(int64(fetchJobDeadline.Seconds())),
		PodFailurePolicy: &batchv1.PodFailurePolicy{Rules: []batchv1.PodFailurePolicyRule{
			{
				Action: batchv1.PodFailurePolicyActionFailJob,
				OnExitCodes: &batchv1.PodFailurePolicyOnExitCodesRequirement{
					ContainerName: ptr.To(FetcherContainerName),
					Operator:      batchv1.PodFailurePolicyOnExitCodesOpIn,
					Values:        []int32{artifact.ExitCodeInvalidArtifact},
				},
			},
			{
				Action: batchv1.PodFailurePolicyActionIgnore,
				OnPodConditions: []batchv1.PodFailurePolicyOnPodConditionsPattern{
					{Type: corev1.DisruptionTarget, Status: corev1.ConditionTrue},
				},
			},
		}},
		Template: corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{Labels: labels},
			Spec: corev1.PodSpec{
				RestartPolicy:                corev1.RestartPolicyNever,
				AutomountServiceAccountToken: ptr.To(false),
				EnableServiceLinks:           ptr.To(false),
				SecurityContext: &corev1.PodSecurityContext{
					RunAsNonRoot:   ptr.To(true),
					SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
				},
				Containers: []corev1.Container{container},
				Volumes:    volumes,
			},
		},
	}

	return job
}

func secretEnvVar(name, secret, key string) corev1.EnvVar {
	return corev1.EnvVar{
		Name: name,
		ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: secret},
			Key:                  key,
		}},
	}
}

func emptyFetchJob(vmImport *v1alpha1.VirtualMachineImport) *batchv1.Job {
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: vmImport.Namespace,
			Name:      apimachinery.GetStableName(vmImport.Name, string(vmImport.UID), fetchJobSuffix),
		},
	}
}

func newManifestConfigMap(
	vmImport *v1alpha1.VirtualMachineImport, fetchResult *artifact.FetchResult, target *unstructured.Unstructured,
) (*corev1.ConfigMap, error) {
	targetJSON, err := target.MarshalJSON()
	if err != nil {
		return nil, err
	}
	withoutConfig := *fetchResult
	withoutConfig.Config = nil
	resultJSON, err := json.Marshal(&withoutConfig)
	if err != nil {
		return nil, err
	}
	if size := len(targetJSON) + len(resultJSON); size > corev1.MaxSecretSize {
		return nil, fmt.Errorf("the import target is %d bytes, at most %d are supported", size, corev1.MaxSecretSize)
	}

	cm := emptyManifestConfigMap(vmImport)
	cm.Labels = map[string]string{v1alpha1.LabelImportUID: string(vmImport.UID)}
	cm.Data = map[string]string{
		ManifestKeyTarget: string(targetJSON),
		ManifestKeyResult: string(resultJSON),
	}

	return cm, nil
}

func emptyManifestConfigMap(vmImport *v1alpha1.VirtualMachineImport) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: vmImport.Namespace,
			Name:      apimachinery.GetStableName(vmImport.Name, string(vmImport.UID), manifestSuffix),
		},
	}
}

func newDataVolume(
	vmImport *v1alpha1.VirtualMachineImport, fetchResult *artifact.FetchResult, layer *artifact.Layer,
) (*cdiv1beta1.DataVolume, error) {
	registry := vmImport.Spec.Source.Registry
	pinnedURL, err := pinRegistryURL(registry.URL, fetchResult.Digest)
	if err != nil {
		return nil, err
	}
	size, err := resource.ParseQuantity(layer.DiskSize)
	if err != nil {
		return nil, fmt.Errorf("disk %q has an invalid size %q: %w", layer.DiskName, layer.DiskSize, err)
	}

	source := &cdiv1beta1.DataVolumeSourceRegistry{
		URL:           &pinnedURL,
		SecretRef:     registry.SecretRef,
		CertConfigMap: registry.CertConfigMap,
		Layer: &cdiv1beta1.LayerSelector{
			MatchAnnotations: map[string]string{artifact.AnnotationDiskName: layer.DiskName},
		},
	}
	// CDI ignores the platform of a plain manifest, and the digest already selects it.
	if fetchResult.IsIndex {
		source.Platform = &cdiv1beta1.PlatformOptions{Architecture: fetchResult.Architecture}
	}

	dv := emptyDataVolume(vmImport.Namespace, getDataVolumeName(vmImport, layer.DiskName))
	dv.Annotations = map[string]string{
		annImmediateBinding: "",
	}
	dv.Labels = map[string]string{
		v1alpha1.LabelImportUID: string(vmImport.UID),
	}
	dv.Spec = cdiv1beta1.DataVolumeSpec{
		Source: &cdiv1beta1.DataVolumeSource{Registry: source},
		Storage: &cdiv1beta1.StorageSpec{
			StorageClassName: vmImport.Spec.StorageClassName,
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: size},
			},
		},
	}

	return dv, nil
}

func emptyDataVolume(namespace, name string) *cdiv1beta1.DataVolume {
	return &cdiv1beta1.DataVolume{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace,
			Name:      name,
		},
	}
}

// parseRegistryURL parses a docker:// URL the way CDI does.
func parseRegistryURL(registryURL string) (reference.Named, error) {
	if !strings.HasPrefix(registryURL, dockerScheme) {
		return nil, fmt.Errorf("registry url %q has to start with %s", registryURL, dockerScheme)
	}
	named, err := reference.ParseNormalizedNamed(strings.TrimPrefix(registryURL, dockerScheme))
	if err != nil {
		return nil, fmt.Errorf("invalid registry url %q: %w", registryURL, err)
	}
	return named, nil
}

// pinRegistryURL replaces the tag or digest of registryURL with d, so every DataVolume reads
// the artifact the fetcher inspected, even if the tag moves meanwhile.
func pinRegistryURL(registryURL string, d digest.Digest) (string, error) {
	named, err := parseRegistryURL(registryURL)
	if err != nil {
		return "", err
	}
	pinned, err := reference.WithDigest(reference.TrimNamed(named), d)
	if err != nil {
		return "", fmt.Errorf("invalid digest %q: %w", d, err)
	}
	return dockerScheme + pinned.String(), nil
}

func getTargetName(vmImport *v1alpha1.VirtualMachineImport) string {
	if name := ptr.Deref(vmImport.Spec.TargetName, ""); name != "" {
		return name
	}
	return vmImport.Name
}

func getDataVolumeName(vmImport *v1alpha1.VirtualMachineImport, diskName string) string {
	return apimachinery.GetStableName(getTargetName(vmImport), string(vmImport.UID), diskName)
}

// SetupWithManager sets up the controller with the Manager.
func (r *VirtualMachineImportReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// Add indexer required for EnqueueRequestByUID
	err := mgr.GetFieldIndexer().IndexField(
		context.Background(), &v1alpha1.VirtualMachineImport{}, uidField,
		func(obj client.Object) []string {
			return []string{string(obj.GetUID())}
		},
	)
	if err != nil {
		return err
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.VirtualMachineImport{}).
		Named(importapi.SingularResourceName).
		Watches(&corev1.Pod{}, handler.EnqueueRequestsFromMapFunc(r.EnqueueRequestByUID)).
		Owns(&cdiv1beta1.DataVolume{}).
		Owns(&batchv1.Job{}).
		Owns(&corev1.ConfigMap{}).
		Complete(r)
}

// EnqueueRequestByUID maps an object labeled with an import UID to that import. The
// metadata-fetch Pods are owned by their Job, not by the import.
func (r *VirtualMachineImportReconciler) EnqueueRequestByUID(ctx context.Context, obj client.Object) []reconcile.Request {
	log := logf.FromContext(ctx)

	importUID, ok := obj.GetLabels()[v1alpha1.LabelImportUID]
	if !ok {
		return nil
	}

	list := &v1alpha1.VirtualMachineImportList{}
	if err := r.List(ctx, list, client.MatchingFields{uidField: importUID}); err != nil {
		log.Error(err, "Unable to list VirtualMachineImports")
		return nil
	}
	if count := len(list.Items); count != 1 {
		if count > 1 {
			log.V(logs.DebugLevel).Info("Expected exactly one VirtualMachineImport", logUID, importUID, logCount, count)
		}
		return nil
	}

	namespace := list.Items[0].Namespace
	name := list.Items[0].Name
	log.V(logs.TraceLevel).Info("Enqueueing VirtualMachineImport", logNS, namespace, logName, name)
	return []reconcile.Request{{
		NamespacedName: types.NamespacedName{
			Namespace: namespace,
			Name:      name,
		},
	}}
}
