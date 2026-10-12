# AGENTS.md - internal/controller

Reconciler for `VirtualMachineImport`, and the availability controller that starts it.

## VirtualMachineImport controller

Multi-step pipeline, every step derived from observed state so it survives restarts:

1. Create the metadata-fetch `Job` running `virt-import-fetcher` (registry credentials via
   `secretKeyRef` env vars, CA ConfigMap mounted, `--insecure-tls` when CDI's `CDIConfig` lists the
   registry). Report Pod start problems and retried failures in `Ready`
2. When the Job completes, read the single `{"fetchResult":...}` line from the logs of its
   succeeded Pod, rewrite the config blob into the target and persist it in the manifest
   `ConfigMap` (keys `target` and `result`), then delete the Job. Exit code 2 / a failed Job is
   terminal
3. Create a `DataVolume` per disk: URL pinned to the fetched digest, `platform` only for an image
   index, `layer.matchAnnotations` on `io.kubevirt.disk.name`, immediate binding
4. Wait for all `DataVolume`s to succeed (event-driven requeue; no fixed interval)
5. Create the `VirtualMachine` or `VirtualMachineTemplate` from the manifest `ConfigMap`
6. On the next reconcile: remove the `DataVolume` owner references from the PVCs, delete the
   `DataVolume`s and the finalizer

Key patterns - preserve these when touching the reconciler:

- Finalizer `import.kubevirt.io/ImportCleanup`. Deleting an incomplete import deletes Job and
  DataVolumes (and with them the PVCs); deleting a completed one only deletes the CR
- `Progressing=True` + `Ready=False` means in progress, with the step as reason
  (`FetchingMetadata`, `ImportingDisks`, `CreatingTarget`); transient errors keep that reason and
  land in the `Ready` message. `Progressing=False` + `Ready=False` with `ImportFailed` is permanent
  (`failImport`), `Progressing=False` + `Ready=True` with `ImportComplete` is done
- Child objects carry `import.kubevirt.io/ImportUID`; an existing object without it is a
  collision and fails the import. Pods are mapped back with `EnqueueRequestByUID`
- Deterministic child names via `GetStableName` (`internal/apimachinery/naming.go`)
- Spec is immutable (CEL rule: `self == oldSelf`)
- Targets are read and created as `unstructured`, so no informer is started for them

## Test patterns

- Tests are split into focused files (`vmimport_job_handling_test.go`, `vmimport_manifest_test.go`,
  `vmimport_datavolume_handling_test.go`, `vmimport_target_handling_test.go`,
  `vmimport_finalizer_test.go`, ...)
- envtest runs no Job controller, kubelet or CDI: tests set Job, Pod and DataVolume status
  themselves, and `fakePodLogReader` stands in for Pod logs
- Shared helpers live in `vmimport_common_test.go`: `createImport()`, `completeFetch()`,
  `fixtureResult()`, `setJobComplete()`, `createFetchPod()`, `completeDataVolumes()`,
  `expectCondition()`
- `config/crd/testing/` holds the CDI `DataVolume` and `CDIConfig` CRDs, the virt-template CRD and a
  minimal `VirtualMachine` CRD (virt-operator creates the real one at runtime)
- Prefer `DescribeTable` with `Entry` for parameterized test cases
