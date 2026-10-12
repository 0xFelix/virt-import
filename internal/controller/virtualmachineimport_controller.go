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
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
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
	uidField  = "metadata.uid"
	nameField = "metadata.name"

	capabilityAll = "ALL"

	FetcherContainerName = "fetcher"
	fetchJobSuffix       = "fetch"
	fetchJobBackoffLimit = 3
	fetchJobDeadline     = 10 * time.Minute
	certsVolumeName      = "registry-certs"
	certsMountPath       = "/etc/virt-import/certs"
	tmpVolumeName        = "tmp"
	tmpMountPath         = "/tmp"

	// The Secret keys CDI uses for registry credentials.
	keyAccessKeyID = "accessKeyId"
	keySecretKey   = "secretKey"

	manifestSuffix = "manifest"
	// ManifestKeyTarget holds the VirtualMachine or VirtualMachineTemplate to create.
	ManifestKeyTarget = "target"
	// ManifestKeyResult holds the artifact.FetchResult, without the config blob.
	ManifestKeyResult = "result"

	cdiConfigName       = "config"
	annImmediateBinding = "cdi.kubevirt.io/storage.bind.immediate.requested"
	annValueTrue        = "true"

	logNS            = "ns"
	logName          = "name"
	logObjectName    = "objectName"
	logConfigMapName = "configMapName"
	logTargetName    = "targetName"
	logJobName       = "jobName"
	logDVName        = "dvName"
	logKind          = "kind"
	logStatus        = "status"
	logReason        = "reason"
	logMessage       = "message"
	logUID           = "uid"
	logCount         = "count"
	logURL           = "url"
	logFinalizer     = "finalizer"
)

var (
	// minDataVolumeSize is the smallest storage request CDI accepts for a DataVolume.
	minDataVolumeSize = resource.MustParse("1Mi")

	// fetcherResources has limits too, so the fetcher is admitted under a ResourceQuota on limits.
	fetcherResources = corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("10m"),
			corev1.ResourceMemory: resource.MustParse("64Mi"),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("100m"),
			corev1.ResourceMemory: resource.MustParse("128Mi"),
		},
	}
)

// VirtualMachineImportReconciler reconciles a VirtualMachineImport object.
type VirtualMachineImportReconciler struct {
	client.Client
	// APIReader reads from the API server what the label-scoped cache cannot show or has not
	// caught up with yet.
	APIReader    client.Reader
	Scheme       *runtime.Scheme
	PodLogs      PodLogReader
	FetcherImage string
}

// +kubebuilder:rbac:groups=import.kubevirt.io,resources=virtualmachineimports,verbs=get;list;watch;patch
// +kubebuilder:rbac:groups=import.kubevirt.io,resources=virtualmachineimports/status,verbs=get;patch
// +kubebuilder:rbac:groups=import.kubevirt.io,resources=virtualmachineimports/finalizers,verbs=update
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups="",resources=pods,verbs=list;watch
// +kubebuilder:rbac:groups="",resources=pods/log,verbs=get
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups=cdi.kubevirt.io,resources=datavolumes,verbs=get;list;watch;create;patch;delete
// +kubebuilder:rbac:groups=cdi.kubevirt.io,resources=cdiconfigs,verbs=get;list;watch
// +kubebuilder:rbac:groups=kubevirt.io,resources=virtualmachines,verbs=get;create
// +kubebuilder:rbac:groups=kubevirt.io,resources=virtualmachines/finalizers,verbs=update
// +kubebuilder:rbac:groups=template.kubevirt.io,resources=virtualmachinetemplates,verbs=get;create
// +kubebuilder:rbac:groups=template.kubevirt.io,resources=virtualmachinetemplates/finalizers,verbs=update

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
		return ctrl.Result{}, r.finalize(ctx, vmImport)
	}
	if err := r.addFinalizer(ctx, vmImport); err != nil {
		return ctrl.Result{}, err
	}

	if !shouldReconcile(vmImport) {
		log.V(logs.DebugLevel).Info("VirtualMachineImport is no longer progressing, not reconciling")
		return ctrl.Result{}, r.handOver(ctx, vmImport)
	}

	helper, err := patch.NewHelper(vmImport, r.Client)
	if err != nil {
		return ctrl.Result{}, err
	}
	// Progressing keeps the step of the previous reconcile until this one reaches another.
	setProgressingCondition(ctx, vmImport, metav1.ConditionTrue, currentStep(vmImport))
	defer func() {
		// Nothing can be created in a terminating namespace, and the import goes away with it.
		if k8serrors.HasStatusCause(retErr, corev1.NamespaceTerminatingCause) {
			log.V(logs.DebugLevel).Info("Namespace is terminating, not reconciling")
			retErr = nil
			return
		}
		setStatusConditions(ctx, vmImport, retErr)
		retErr = errors.Join(retErr, helper.Patch(ctx, vmImport))
	}()

	if validateErr := validateImport(vmImport); validateErr != nil {
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
	// A second reconcile of a deleted import can still see it in the cache.
	return client.IgnoreNotFound(r.Patch(ctx, vmImport, client.MergeFrom(vmImportCopy)))
}

// finalize cleans up a deleted import and removes the finalizer.
func (r *VirtualMachineImportReconciler) finalize(
	ctx context.Context, vmImport *v1alpha1.VirtualMachineImport,
) error {
	if !controllerutil.ContainsFinalizer(vmImport, v1alpha1.FinalizerImportCleanup) {
		return nil
	}

	logf.FromContext(ctx).V(logs.TraceLevel).Info("Finalizing VirtualMachineImport")
	if err := r.deleteImportResources(ctx, vmImport); err != nil {
		return err
	}
	return r.removeFinalizer(ctx, vmImport)
}

// getCreatedTarget returns the target recorded in the manifest ConfigMap if it exists and was
// created by vmImport, else nil.
func (r *VirtualMachineImportReconciler) getCreatedTarget(
	ctx context.Context, vmImport *v1alpha1.VirtualMachineImport,
) (*unstructured.Unstructured, error) {
	cm := emptyManifestConfigMap(vmImport)
	if err := r.Get(ctx, client.ObjectKeyFromObject(cm), cm); err != nil {
		return nil, client.IgnoreNotFound(err)
	}
	if !belongsTo(cm, vmImport) {
		return nil, nil
	}
	// An invalid manifest failed the import before its target could be created.
	_, target, err := decodeManifest(cm)
	if err != nil {
		return nil, nil
	}

	existing, err := r.getExistingTarget(ctx, target)
	switch {
	case meta.IsNoMatchError(err):
		return nil, nil
	case err != nil:
		return nil, err
	case existing == nil || !belongsTo(existing, vmImport):
		return nil, nil
	default:
		return existing, nil
	}
}

// getExistingTarget reads the object target describes, nil if there is none.
func (r *VirtualMachineImportReconciler) getExistingTarget(
	ctx context.Context, target *unstructured.Unstructured,
) (*unstructured.Unstructured, error) {
	existing := &unstructured.Unstructured{}
	existing.SetGroupVersionKind(target.GroupVersionKind())
	if err := r.Get(ctx, client.ObjectKeyFromObject(target), existing); err != nil {
		return nil, client.IgnoreNotFound(err)
	}
	return existing, nil
}

// handOver hands the DataVolumes of a completed import over to its target and deletes everything
// else it created, keeping the finalizer. Once done, it only reads the cache. A failed import keeps
// its resources until it is deleted.
func (r *VirtualMachineImportReconciler) handOver(
	ctx context.Context, vmImport *v1alpha1.VirtualMachineImport,
) error {
	if !isReady(vmImport) {
		return nil
	}
	return r.deleteImportResources(ctx, vmImport)
}

// deleteImportResources deletes the Job and the manifest ConfigMap of an import. Its DataVolumes
// are handed over to its target if it created one, like virt-template hands them over to the
// VirtualMachineTemplate, otherwise they are deleted with their PVCs.
func (r *VirtualMachineImportReconciler) deleteImportResources(
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
	var target *unstructured.Unstructured
	if len(dvs) > 0 {
		if target, err = r.getCreatedTarget(ctx, vmImport); err != nil {
			return err
		}
	}
	for i := range dvs {
		dv := &dvs[i]
		if target != nil {
			if err := r.handOverDataVolume(ctx, vmImport, dv, target); err != nil {
				log.Error(err, "Unable to hand over DataVolume to the import target", logNS, dv.Namespace, logDVName, dv.Name)
				return err
			}
			continue
		}
		// The cache can still show a DataVolume as controlled by vmImport after it was handed
		// over and the manifest ConfigMap is gone. The preconditions turn deleting such a stale
		// DataVolume into a conflict, and the retry sees it handed over.
		log.Info("Deleting DataVolume", logNS, dv.Namespace, logDVName, dv.Name)
		if err := client.IgnoreNotFound(r.Delete(ctx, dv, client.Preconditions{
			UID: ptr.To(dv.UID), ResourceVersion: ptr.To(dv.ResourceVersion),
		})); err != nil {
			return err
		}
	}

	// finalize reads the target from the manifest until the DataVolumes are gone.
	return r.deleteChild(ctx, vmImport, emptyManifestConfigMap(vmImport))
}

// handOverDataVolume makes target the controller of dv instead of vmImport.
func (r *VirtualMachineImportReconciler) handOverDataVolume(
	ctx context.Context, vmImport *v1alpha1.VirtualMachineImport,
	dv *cdiv1beta1.DataVolume, target *unstructured.Unstructured,
) error {
	logf.FromContext(ctx).V(logs.DebugLevel).Info("Handing over DataVolume to the import target",
		logNS, dv.Namespace, logDVName, dv.Name)
	dvCopy := dv.DeepCopy()
	if err := controllerutil.RemoveControllerReference(vmImport, dv, r.Scheme); err != nil {
		return err
	}
	if err := controllerutil.SetControllerReference(target, dv, r.Scheme); err != nil {
		return err
	}
	return r.Patch(ctx, dv, client.MergeFrom(dvCopy))
}

// listDataVolumes lists the DataVolumes vmImport controls, not those already handed over.
func (r *VirtualMachineImportReconciler) listDataVolumes(
	ctx context.Context, vmImport *v1alpha1.VirtualMachineImport,
) ([]cdiv1beta1.DataVolume, error) {
	list := &cdiv1beta1.DataVolumeList{}
	if err := r.List(ctx, list, client.InNamespace(vmImport.Namespace),
		client.MatchingLabels{v1alpha1.LabelImportUID: string(vmImport.UID)}); err != nil {
		return nil, err
	}
	return slices.DeleteFunc(list.Items, func(dv cdiv1beta1.DataVolume) bool {
		return !metav1.IsControlledBy(&dv, vmImport)
	}), nil
}

func (r *VirtualMachineImportReconciler) deleteFetchJob(
	ctx context.Context, vmImport *v1alpha1.VirtualMachineImport,
) error {
	return r.deleteChild(ctx, vmImport, emptyFetchJob(vmImport))
}

// deleteChild deletes obj if it exists and belongs to vmImport. It checks the cache first, so
// repeated calls for an object that is gone cost no API request.
func (r *VirtualMachineImportReconciler) deleteChild(
	ctx context.Context, vmImport *v1alpha1.VirtualMachineImport, obj client.Object,
) error {
	if err := r.Get(ctx, client.ObjectKeyFromObject(obj), obj); err != nil {
		return client.IgnoreNotFound(err)
	}
	if !belongsTo(obj, vmImport) {
		return nil
	}

	err := r.Delete(ctx, obj, client.PropagationPolicy(metav1.DeletePropagationBackground))
	if err == nil {
		logf.FromContext(ctx).V(logs.DebugLevel).Info("Deleted "+r.kindOf(obj), logNS, obj.GetNamespace(), logObjectName, obj.GetName())
	}
	return client.IgnoreNotFound(err)
}

// getChild reads obj and reports whether it exists. It fails the import if obj exists but was
// not created by it.
func (r *VirtualMachineImportReconciler) getChild(
	ctx context.Context, vmImport *v1alpha1.VirtualMachineImport, obj client.Object,
) (bool, error) {
	if err := r.Get(ctx, client.ObjectKeyFromObject(obj), obj); err != nil {
		return false, client.IgnoreNotFound(err)
	}
	return true, r.checkOwnership(ctx, vmImport, obj)
}

// createChild creates obj, a child of vmImport, and returns it. Creating an object the cache
// did not show can conflict with one the cache has not caught up with yet, or with a foreign
// one, which the label-scoped cache never shows. On a conflict, the existing object is read from
// the API server and returned, and the import fails unless the object belongs to it.
func createChild[T any, PT interface {
	*T
	client.Object
}](ctx context.Context, r *VirtualMachineImportReconciler, vmImport *v1alpha1.VirtualMachineImport, obj PT) (PT, error) {
	err := r.Create(ctx, obj)
	if !k8serrors.IsAlreadyExists(err) {
		return obj, err
	}

	existing := PT(new(T))
	if err := r.APIReader.Get(ctx, client.ObjectKeyFromObject(obj), existing); err != nil {
		return nil, err
	}
	if err := r.checkOwnership(ctx, vmImport, existing); err != nil {
		return nil, err
	}
	return existing, nil
}

func (r *VirtualMachineImportReconciler) checkOwnership(
	ctx context.Context, vmImport *v1alpha1.VirtualMachineImport, obj client.Object,
) error {
	if belongsTo(obj, vmImport) {
		return nil
	}
	return failImport(ctx, vmImport, fmt.Errorf("%s %s/%s does not belong to this import",
		r.kindOf(obj), obj.GetNamespace(), obj.GetName()))
}

func (r *VirtualMachineImportReconciler) kindOf(obj client.Object) string {
	gvk, err := apiutil.GVKForObject(obj, r.Scheme)
	if err != nil {
		return fmt.Sprintf("%T", obj)
	}
	return gvk.Kind
}

// reconcileManifest returns the manifest ConfigMap of the import. Until it exists, it runs the
// metadata-fetch Job and creates the ConfigMap from the Job's result.
func (r *VirtualMachineImportReconciler) reconcileManifest(
	ctx context.Context, vmImport *v1alpha1.VirtualMachineImport,
) (*corev1.ConfigMap, *ctrl.Result, error) {
	cm := emptyManifestConfigMap(vmImport)
	if found, err := r.getChild(ctx, vmImport, cm); err != nil {
		return nil, nil, err
	} else if found {
		// The Job is deleted only once the cache shows the ConfigMap. Deleting it earlier lets a
		// reconcile that sees neither of them start another fetch.
		return cm, nil, r.deleteFetchJob(ctx, vmImport)
	}

	setProgressingCondition(ctx, vmImport, metav1.ConditionTrue, v1alpha1.ReasonFetchingMetadata)

	job := emptyFetchJob(vmImport)
	if found, err := r.getChild(ctx, vmImport, job); err != nil {
		return nil, nil, err
	} else if !found {
		if createErr := r.createFetchJob(ctx, vmImport); createErr != nil {
			return nil, nil, createErr
		}
		setReadyCondition(ctx, vmImport, metav1.ConditionFalse, v1alpha1.ReasonFetchingMetadata,
			"Fetching artifact metadata from %s", vmImport.Spec.Source.Registry.URL)
		return nil, &ctrl.Result{}, nil
	}

	pods, err := listFetchPods(ctx, r.Client, job)
	if err != nil {
		return nil, nil, err
	}
	if failed, message := isJobConditionTrue(job, batchv1.JobFailed); failed {
		return nil, nil, failImport(ctx, vmImport,
			fmt.Errorf("fetching artifact metadata failed: %s", cmp.Or(terminationMessage(pods), message)))
	}
	if complete, _ := isJobConditionTrue(job, batchv1.JobComplete); !complete {
		syncFetchPodStatus(ctx, vmImport, pods)
		return nil, &ctrl.Result{}, nil
	}

	cm, err = r.createManifestConfigMap(ctx, vmImport, job, pods)
	// Without a ConfigMap and an error, the result was lost and the fetch starts over.
	if cm == nil || err != nil {
		return nil, &ctrl.Result{}, err
	}
	return cm, nil, nil
}

func (r *VirtualMachineImportReconciler) createFetchJob(
	ctx context.Context, vmImport *v1alpha1.VirtualMachineImport,
) error {
	insecure, err := r.isInsecureRegistry(ctx, vmImport.Spec.Source.Registry.URL)
	if err != nil {
		return err
	}

	job := newFetchJob(vmImport, r.FetcherImage, insecure)
	err = ctrl.SetControllerReference(vmImport, job, r.Scheme)
	if err != nil {
		return err
	}
	logf.FromContext(ctx).Info("Creating metadata-fetch Job", logNS, job.Namespace, logJobName, job.Name,
		logURL, vmImport.Spec.Source.Registry.URL)
	_, err = createChild(ctx, r, vmImport, job)
	return err
}

// isInsecureRegistry reports whether CDI treats the registry of url as insecure. The fetcher
// follows CDI, so the metadata and the disks are read the same way. CDI matches the host of the
// pinned DataVolume URL, which is the domain of the normalized reference, also for short names.
func (r *VirtualMachineImportReconciler) isInsecureRegistry(ctx context.Context, registryURL string) (bool, error) {
	cdiConfig := &cdiv1beta1.CDIConfig{}
	if err := r.Get(ctx, client.ObjectKey{Name: cdiConfigName}, cdiConfig); err != nil {
		return false, client.IgnoreNotFound(err)
	}

	named, err := parseRegistryURL(registryURL)
	if err != nil {
		return false, err
	}
	return slices.Contains(cdiConfig.Spec.InsecureRegistries, reference.Domain(named)), nil
}

// listFetchPods lists the Pods of job with reader. The Job selector matches only the Pods of this
// incarnation of the Job, not those of an earlier Job of the same name, and the controller
// reference skips Pods that others labeled alike.
func listFetchPods(ctx context.Context, reader client.Reader, job *batchv1.Job) ([]corev1.Pod, error) {
	selector, err := metav1.LabelSelectorAsSelector(job.Spec.Selector)
	if err != nil {
		return nil, err
	}
	list := &corev1.PodList{}
	if err := reader.List(ctx, list, client.InNamespace(job.Namespace),
		client.MatchingLabelsSelector{Selector: selector}); err != nil {
		return nil, err
	}
	return slices.DeleteFunc(list.Items, func(pod corev1.Pod) bool {
		return !metav1.IsControlledBy(&pod, job)
	}), nil
}

// createManifestConfigMap reads the fetch result from the logs of the succeeded fetch Pod and
// persists the target built from it, so the import survives the Job and controller restarts.
// If the succeeded Pod is gone, it deletes the Job to fetch again and returns no ConfigMap.
func (r *VirtualMachineImportReconciler) createManifestConfigMap(
	ctx context.Context, vmImport *v1alpha1.VirtualMachineImport, job *batchv1.Job, pods []corev1.Pod,
) (*corev1.ConfigMap, error) {
	pod := findPod(pods, corev1.PodSucceeded)
	if pod == nil {
		// The Pod cache can lag behind the Job, only the API server tells whether the Pod is gone.
		uncached, err := listFetchPods(ctx, r.APIReader, job)
		if err != nil {
			return nil, err
		}
		pod = findPod(uncached, corev1.PodSucceeded)
	}
	if pod == nil {
		return nil, r.restartFetch(ctx, vmImport, job, "its succeeded Pod is gone")
	}
	podLogs, err := r.PodLogs.GetLogs(ctx, pod.Namespace, pod.Name, FetcherContainerName)
	if k8serrors.IsNotFound(err) {
		return nil, r.restartFetch(ctx, vmImport, job, fmt.Sprintf("Pod %s/%s is gone", pod.Namespace, pod.Name))
	} else if err != nil {
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
	logf.FromContext(ctx).Info("Creating manifest ConfigMap", logNS, cm.Namespace, logConfigMapName, cm.Name)
	return createChild(ctx, r, vmImport, cm)
}

// restartFetch deletes the completed Job whose result cannot be read anymore, so the next
// reconcile fetches the metadata again.
func (r *VirtualMachineImportReconciler) restartFetch(
	ctx context.Context, vmImport *v1alpha1.VirtualMachineImport, job *batchv1.Job, reason string,
) error {
	logf.FromContext(ctx).Info("Restarting the metadata fetch", logJobName, job.Name, logReason, reason)
	if err := r.deleteFetchJob(ctx, vmImport); err != nil {
		return err
	}
	setReadyCondition(ctx, vmImport, metav1.ConditionFalse, v1alpha1.ReasonFetchingMetadata,
		"Fetching artifact metadata from %s again, the result of Job %s is lost: %s",
		vmImport.Spec.Source.Registry.URL, job.Name, reason)
	return nil
}

// checkTargetAvailable fails the import if its target already exists and was not created by it,
// or if the API serving the target is missing.
func (r *VirtualMachineImportReconciler) checkTargetAvailable(
	ctx context.Context, vmImport *v1alpha1.VirtualMachineImport, target *unstructured.Unstructured,
) error {
	existing, err := r.getExistingTarget(ctx, target)
	switch {
	case meta.IsNoMatchError(err):
		return failImport(ctx, vmImport, fmt.Errorf("API group %s is not available", target.GroupVersionKind().Group))
	case err != nil:
		return err
	case existing != nil && !belongsTo(existing, vmImport):
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

	vmImport.Status.DiskImports = make([]v1alpha1.DiskImportStatus, 0, len(fetchResult.Layers))
	var waitingFor *cdiv1beta1.DataVolume
	for _, layer := range fetchResult.Layers {
		dv, err := r.ensureDataVolume(ctx, vmImport, fetchResult, &layer)
		if err != nil {
			return false, err
		}
		vmImport.Status.DiskImports = append(vmImport.Status.DiskImports, v1alpha1.DiskImportStatus{
			Name:           layer.DiskName,
			DataVolumeName: dv.Name,
			Phase:          string(dv.Status.Phase),
			Progress:       string(dv.Status.Progress),
		})
		if dv.Status.Phase == cdiv1beta1.Succeeded {
			continue
		}
		// A DataVolume is failed the way virt-template judges it. While CDI retries a failed
		// import, which it does without ever failing the DataVolume, Running tells why.
		if notRunning := notRunningCondition(dv); notRunning != nil && dv.Status.Phase == cdiv1beta1.Failed {
			return false, failImport(ctx, vmImport, fmt.Errorf("dataVolume %s/%s failed: %s", dv.Namespace, dv.Name, notRunning.Message))
		}
		if waitingFor == nil {
			waitingFor = dv
		}
	}

	if waitingFor != nil {
		message := fmt.Sprintf("Waiting for DataVolume %s/%s to be ready", waitingFor.Namespace, waitingFor.Name)
		if notRunning := notRunningCondition(waitingFor); notRunning != nil && notRunning.Message != "" {
			message += ": " + notRunning.Message
		}
		setReadyCondition(ctx, vmImport, metav1.ConditionFalse, v1alpha1.ReasonImportingDisks, "%s", message)
		return false, nil
	}
	return true, nil
}

func (r *VirtualMachineImportReconciler) ensureDataVolume(
	ctx context.Context, vmImport *v1alpha1.VirtualMachineImport,
	fetchResult *artifact.FetchResult, layer *artifact.Layer,
) (*cdiv1beta1.DataVolume, error) {
	dv := emptyDataVolume(vmImport, layer.DiskName)
	if found, err := r.getChild(ctx, vmImport, dv); found || err != nil {
		return dv, err
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
	dv, err = createChild(ctx, r, vmImport, dv)
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

func (r *VirtualMachineImportReconciler) reconcileTarget(
	ctx context.Context, vmImport *v1alpha1.VirtualMachineImport,
	target *unstructured.Unstructured, diskCount int,
) error {
	setProgressingCondition(ctx, vmImport, metav1.ConditionTrue, v1alpha1.ReasonCreatingTarget)

	logf.FromContext(ctx).Info("Creating import target", logKind, target.GetKind(), logNS, target.GetNamespace(),
		logTargetName, target.GetName())
	err := r.Create(ctx, target)
	// The target can exist from before a restart or belong to someone else, or its API can be gone.
	if k8serrors.IsAlreadyExists(err) || meta.IsNoMatchError(err) {
		err = r.checkTargetAvailable(ctx, vmImport, target)
	}
	if err != nil {
		return err
	}

	setTargetRef(vmImport, target)
	setReadyCondition(ctx, vmImport, metav1.ConditionTrue, v1alpha1.ReasonImportComplete,
		"%s %s created with %d disk(s)", target.GetKind(), target.GetName(), diskCount)
	setProgressingCondition(ctx, vmImport, metav1.ConditionFalse, v1alpha1.ReasonImportComplete)

	return nil
}

// shouldReconcile is false once Progressing is False: a terminal import is never restarted.
func shouldReconcile(vmImport *v1alpha1.VirtualMachineImport) bool {
	progressing := meta.FindStatusCondition(vmImport.Status.Conditions, v1alpha1.ConditionProgressing)

	return progressing == nil ||
		progressing.Status != metav1.ConditionFalse
}

func belongsTo(obj client.Object, vmImport *v1alpha1.VirtualMachineImport) bool {
	return obj.GetLabels()[v1alpha1.LabelImportUID] == string(vmImport.UID)
}

func isReady(vmImport *v1alpha1.VirtualMachineImport) bool {
	return meta.IsStatusConditionTrue(vmImport.Status.Conditions, v1alpha1.ConditionReady)
}

// validateImport checks what the CRD cannot, the CEL rules already require a docker:// registry URL.
func validateImport(vmImport *v1alpha1.VirtualMachineImport) error {
	_, err := parseRegistryURL(vmImport.Spec.Source.Registry.URL)
	return err
}

// failImport marks the import as permanently failed. The returned error ends up in the Ready
// condition through setStatusConditions, and is terminal, so the import is not requeued.
func failImport(ctx context.Context, vmImport *v1alpha1.VirtualMachineImport, err error) error {
	setProgressingCondition(ctx, vmImport, metav1.ConditionFalse, v1alpha1.ReasonImportFailed)
	return reconcile.TerminalError(err)
}

// conditionMessage returns the message of err without the prefix of a terminal error.
// failImport errors are returned unwrapped, so the terminal error is err itself.
func conditionMessage(err error) string {
	if errors.Is(err, reconcile.TerminalError(nil)) {
		if inner := errors.Unwrap(err); inner != nil {
			return inner.Error()
		}
	}
	return err.Error()
}

// currentStep returns the reason of the step the import was in after the previous reconcile.
func currentStep(vmImport *v1alpha1.VirtualMachineImport) string {
	progressing := meta.FindStatusCondition(vmImport.Status.Conditions, v1alpha1.ConditionProgressing)
	if progressing == nil || progressing.Status != metav1.ConditionTrue || progressing.Reason == v1alpha1.ReasonWaiting {
		return v1alpha1.ReasonFetchingMetadata
	}
	return progressing.Reason
}

// setStatusConditions reports an error in the Ready condition, or sets Ready if a reconcile left
// it unset. An error keeps the import in its current step; after failImport it is terminal.
func setStatusConditions(ctx context.Context, vmImport *v1alpha1.VirtualMachineImport, retErr error) {
	progressing := meta.FindStatusCondition(vmImport.Status.Conditions, v1alpha1.ConditionProgressing)

	if retErr != nil {
		setReadyCondition(ctx, vmImport, metav1.ConditionFalse, progressing.Reason, "%s", conditionMessage(retErr))
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
		Digest:       fetchResult.Digest.String(),
	}
}

func setTargetRef(vmImport *v1alpha1.VirtualMachineImport, target *unstructured.Unstructured) {
	vmImport.Status.TargetRef = &corev1.TypedLocalObjectReference{
		APIGroup: ptr.To(target.GroupVersionKind().Group),
		Kind:     target.GetKind(),
		Name:     target.GetName(),
	}
}

func syncFetchPodStatus(ctx context.Context, vmImport *v1alpha1.VirtualMachineImport, pods []corev1.Pod) {
	for i := range pods {
		for _, status := range pods[i].Status.ContainerStatuses {
			if waiting := status.State.Waiting; waiting != nil && isFetcherBlocked(waiting.Reason) {
				setReadyCondition(ctx, vmImport, metav1.ConditionFalse, v1alpha1.ReasonFetchingMetadata,
					"Metadata-fetch Pod %s/%s cannot start: %s: %s", pods[i].Namespace, pods[i].Name,
					waiting.Reason, waiting.Message)
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

// isFetcherBlocked reports whether a waiting fetcher needs the user's attention, e.g. a missing Secret.
func isFetcherBlocked(reason string) bool {
	switch reason {
	case "CreateContainerConfigError", "CreateContainerError", "ErrImagePull", "ImagePullBackOff", "InvalidImageName":
		return true
	}
	return false
}

// notRunningCondition returns the Running condition of dv unless it is True.
func notRunningCondition(dv *cdiv1beta1.DataVolume) *cdiv1beta1.DataVolumeCondition {
	for i := range dv.Status.Conditions {
		if cond := &dv.Status.Conditions[i]; cond.Type == cdiv1beta1.DataVolumeRunning {
			if cond.Status == corev1.ConditionTrue {
				return nil
			}
			return cond
		}
	}
	return nil
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

	args := []string{fetcherArg(artifact.FlagURL, registry.URL)}
	if registry.Platform != nil && registry.Platform.Architecture != "" {
		args = append(args, fetcherArg(artifact.FlagArch, registry.Platform.Architecture))
	}
	if insecure {
		args = append(args, fetcherFlag(artifact.FlagInsecureTLS))
	}

	container := corev1.Container{
		Name:  FetcherContainerName,
		Image: image,
		Args:  args,
		VolumeMounts: []corev1.VolumeMount{
			{Name: tmpVolumeName, MountPath: tmpMountPath},
		},
		Resources:                *fetcherResources.DeepCopy(),
		TerminationMessagePolicy: corev1.TerminationMessageReadFile,
		SecurityContext: &corev1.SecurityContext{
			AllowPrivilegeEscalation: ptr.To(false),
			Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{capabilityAll}},
			ReadOnlyRootFilesystem:   ptr.To(true),
		},
	}
	volumes := []corev1.Volume{
		{Name: tmpVolumeName, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
	}
	if registry.SecretRef != "" {
		container.Env = []corev1.EnvVar{
			secretEnvVar(artifact.EnvAccessKeyID, registry.SecretRef, keyAccessKeyID),
			secretEnvVar(artifact.EnvSecretKey, registry.SecretRef, keySecretKey),
		}
	}
	if registry.CertConfigMap != "" {
		container.Args = append(container.Args, fetcherArg(artifact.FlagCertDir, certsMountPath))
		container.VolumeMounts = append(container.VolumeMounts,
			corev1.VolumeMount{Name: certsVolumeName, MountPath: certsMountPath, ReadOnly: true})
		volumes = append(volumes, corev1.Volume{
			Name: certsVolumeName,
			VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: registry.CertConfigMap},
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

func fetcherFlag(name string) string {
	return "--" + name
}

func fetcherArg(name, value string) string {
	return fetcherFlag(name) + "=" + value
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
	// The API server validates ConfigMaps against the size limit of Secrets.
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
	if size.Cmp(minDataVolumeSize) < 0 {
		size = minDataVolumeSize.DeepCopy()
	}

	source := &cdiv1beta1.DataVolumeSourceRegistry{
		URL: &pinnedURL,
		Layer: &cdiv1beta1.LayerSelector{
			MatchAnnotations: map[string]string{artifact.AnnotationDiskName: layer.DiskName},
		},
	}
	if registry.SecretRef != "" {
		source.SecretRef = ptr.To(registry.SecretRef)
	}
	if registry.CertConfigMap != "" {
		source.CertConfigMap = ptr.To(registry.CertConfigMap)
	}
	// CDI ignores the platform of a plain manifest, and the digest already selects it.
	if fetchResult.IsIndex {
		source.Platform = &cdiv1beta1.PlatformOptions{Architecture: fetchResult.Architecture}
	}

	dv := emptyDataVolume(vmImport, layer.DiskName)
	dv.Annotations = map[string]string{
		annImmediateBinding: annValueTrue,
	}
	dv.Labels = map[string]string{
		v1alpha1.LabelImportUID: string(vmImport.UID),
	}
	dv.Spec = cdiv1beta1.DataVolumeSpec{
		Source: &cdiv1beta1.DataVolumeSource{Registry: source},
		Storage: &cdiv1beta1.StorageSpec{
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: size},
			},
		},
	}
	if vmImport.Spec.StorageClassName != "" {
		dv.Spec.Storage.StorageClassName = ptr.To(vmImport.Spec.StorageClassName)
	}

	return dv, nil
}

func emptyDataVolume(vmImport *v1alpha1.VirtualMachineImport, diskName string) *cdiv1beta1.DataVolume {
	return &cdiv1beta1.DataVolume{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: vmImport.Namespace,
			Name:      getDataVolumeName(vmImport, diskName),
		},
	}
}

// parseRegistryURL parses a docker:// URL the way CDI does.
func parseRegistryURL(registryURL string) (reference.Named, error) {
	if !strings.HasPrefix(registryURL, artifact.DockerScheme) {
		return nil, fmt.Errorf("registry url %q has to start with %s", registryURL, artifact.DockerScheme)
	}
	named, err := reference.ParseNormalizedNamed(strings.TrimPrefix(registryURL, artifact.DockerScheme))
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
	return artifact.DockerScheme + pinned.String(), nil
}

func getTargetName(vmImport *v1alpha1.VirtualMachineImport) string {
	if vmImport.Spec.TargetName != "" {
		return vmImport.Spec.TargetName
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
