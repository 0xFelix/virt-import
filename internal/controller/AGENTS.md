# AGENTS.md - internal/controller

Reconciler for `VirtualMachineImport`, and the availability controller that starts it.

## VirtualMachineImport controller

Multi-step pipeline, every step derived from observed state so it survives restarts:

1. Create the metadata-fetch `Job` running `virt-import-fetcher` (registry credentials via
   `secretKeyRef` env vars, CA ConfigMap mounted, `--insecure-tls` when CDI's `CDIConfig` lists the
   registry). Report Pod start problems and retried failures in `Ready`
2. When the Job completes, read the single `{"fetchResult":...}` line from the logs of its
   succeeded Pod (matched by the Job selector and controlled by the Job), rewrite the config
   blob into the target and persist it in the manifest `ConfigMap` (keys `target` and
   `result`). The Job is deleted once the cache shows the ConfigMap, so a lagging cache never
   starts a second fetch.
   Exit code 2 / a failed Job is terminal. If the succeeded Pod or its logs are gone, the Job
   is deleted and the fetch starts over
3. Create a `DataVolume` per disk: URL pinned to the fetched digest, `platform` only for an image
   index, `layer.matchAnnotations` on `io.kubevirt.disk.name`, immediate binding
4. Wait for all `DataVolume`s to succeed (event-driven requeue; no fixed interval), reporting
   the `Running` condition message of a stalled one in `Ready`
5. Create the `VirtualMachine` or `VirtualMachineTemplate` from the manifest `ConfigMap`
6. On the next reconcile (`handOver`): make the target the controller of the `DataVolume`s,
   like virt-template hands them over to the `VirtualMachineTemplate`, so they and their PVCs are
   deleted with it, then delete the manifest `ConfigMap`. If the target is gone, the
   `DataVolume`s are deleted. Handed-over `DataVolume`s keep the `ImportUID` label but are
   ignored. Once done, reconciling a completed import reads only the cache and writes nothing

Key patterns - preserve these when touching the reconciler:

- Finalizer `import.kubevirt.io/ImportCleanup`, kept until the import is deleted, as virt-template
  does: removing it earlier makes the next reconcile add it again. Deleting an import
  deletes Job and manifest `ConfigMap`; the DataVolumes go to the target like in step 6 if the
  import created it, else they are deleted
- `Progressing=True` + `Ready=False` means in progress, with the step as reason
  (`FetchingMetadata`, `ImportingDisks`, `CreatingTarget`); transient errors keep that reason and
  land in the `Ready` message. `Progressing=False` + `Ready=False` with `ImportFailed` is permanent
  (`failImport`, which returns a `reconcile.TerminalError` so the import is not requeued),
  `Progressing=False` + `Ready=True` with `ImportComplete` is done. A reconcile first sets
  `Progressing` to the step of the previous one, and each step it reaches overwrites the reason;
  as the status stays `True`, the transition time stays, so an unchanged import is not written
- Child objects carry `import.kubevirt.io/ImportUID`; an existing object without it is a
  collision and fails the import. The label-scoped cache never shows such an object, so
  `createChild` reads it from the API server (`APIReader`) when a create conflicts; that also
  covers a child the cache has not caught up with yet. Pods are mapped back with
  `EnqueueRequestByUID`
- Deterministic child names via `GetStableName` (`internal/apimachinery/naming.go`)
- Spec immutability, the registry source, its `docker://` URL and object names are CEL rules;
  `validateImport` only adds the image reference check
- Targets are read and created as `unstructured`, so no informer is started for them
- A reconcile in a terminating namespace ends without error, retry or status patch: nothing can
  be created there, and the import goes away with the namespace
- controller-runtime already logs the import as `name` and `namespace`; other objects get their
  own log keys (`configMapName`, `targetName`, `dvName`, `objectName`, ...)

## Test patterns

- Tests are split into focused files (`vmimport_job_handling_test.go`, `vmimport_manifest_test.go`,
  `vmimport_datavolume_handling_test.go`, `vmimport_target_handling_test.go`,
  `vmimport_finalizer_test.go`, ...)
- envtest runs no Job controller, kubelet or CDI: tests set Job, Pod and DataVolume status
  themselves, and `fakePodLogReader` stands in for Pod logs
- Shared helpers live in `vmimport_common_test.go`: `createImport()`, `completeFetch()`,
  `fixtureResult()`, `setJobComplete()`, `createFetchPod()`, `completeDataVolumes()`,
  `expectCondition()`, and `hidingClient()`, which hides one type the way the cache does
- `config/crd/testing/` holds the CDI `DataVolume` and `CDIConfig` CRDs, the virt-template CRD and the
  KubeVirt `VirtualMachine` CRD, pulled from a cluster by `make cluster-envtest-crds` or
  `make kubevirt-envtest-crds`. Objects tests create must be valid against them, e.g.
  `createForeignVM()` for a colliding target
- Prefer `DescribeTable` with `Entry` for parameterized test cases
