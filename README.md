# virt-import

Import `VirtualMachines` and `VirtualMachineTemplates` from OCI artifacts stored in container
registries.

virt-import is the Phase 2 deliverable of
[VEP #395: OCI Artifact Import](https://github.com/kubevirt/enhancements/pull/396). It completes the
round trip started by [VEP #256](https://github.com/kubevirt/enhancements/pull/257) (OCI export):
export a VM on one cluster, push it to a registry, import it on another.

> **Status: alpha, under construction.** The `VirtualMachineImport` API and the controller are
> scaffolded, but the import logic is not implemented yet - creating a `VirtualMachineImport` today
> does nothing. Targeting KubeVirt v1.10.

## How it works

```
VirtualMachineImport CR
  |
  +-- 1. metadata-fetch Job mounts the registry Secret/ConfigMap, resolves the
  |       platform, validates the artifact, and reports artifactType, config
  |       blob and layer info as JSON
  |
  +-- 2. one DataVolume per disk layer; CDI selects the layer by the
  |       io.kubevirt.disk.name annotation and streams the raw blob to a PVC
  |
  +-- 3. config blob is rewritten so volumes point at the created PVCs
  |
  +-- 4. the VirtualMachine or VirtualMachineTemplate is created
```

The target kind is auto-detected from the OCI manifest's `artifactType`. The controller never talks
to a registry itself: credentials stay in the metadata-fetch Job's Pod and in CDI's importer Pods, so
the controller needs no cluster-wide Secret access.

Everything an import creates lands in the namespace of the `VirtualMachineImport`. Imports are
all-or-nothing; if any step fails, no target is created.

## Requirements

- KubeVirt
- CDI, with the `Layer` field on `DataVolumeSourceRegistry` (VEP #395 Phase 1)
- [kubevirt/virt-template](https://github.com/kubevirt/virt-template), only if you import
  `VirtualMachineTemplate` artifacts
- cert-manager, for the standalone install

## Install

```console
$ kubectl apply -f https://github.com/kubevirt/virt-import/releases/latest/download/install.yaml
```

Or build the manifest yourself:

```console
$ make build-installer
$ kubectl apply -f dist/install.yaml
```

For OLM, the controller image ships a `csv-generator`. The hyperconverged-cluster-operator runs it as
the image entrypoint and merges the `ClusterServiceVersion` it prints. `make csv` prints it locally.

## Usage

```yaml
apiVersion: import.kubevirt.io/v1alpha1
kind: VirtualMachineImport
metadata:
  name: my-import
  namespace: default
spec:
  source:
    registry:
      url: docker://registry.example.com/vms/fedora:v1
      secretRef: registry-credentials
      certConfigMap: registry-ca
  targetName: fedora-vm
  storageClassName: ceph-block
```

`targetName` defaults to the name of the `VirtualMachineImport`. PVCs are named
`${targetName}-${diskName}`.

For a multi-arch artifact, select the variant:

```yaml
spec:
  source:
    registry:
      url: docker://registry.example.com/templates/fedora:v1
      platform:
        architecture: arm64
```

A single-manifest artifact is used as-is when `architecture` is unset; a multi-manifest artifact
fails and asks you to pick one.

Watch progress:

```console
$ kubectl get vmimport
NAME        TARGET      AGE   READY   PROGRESSING
my-import   fedora-vm   2m    True    False
```

`status.diskImports` tracks each disk, `status.artifactInfo` reports what was found in the artifact,
and `status.targetRef` points at the created object.

### Cleanup

Deleting a `VirtualMachineImport` **before** it completes removes the intermediate resources. After a
successful import, the created VM or template and its PVCs are independent - deleting the
`VirtualMachineImport` only removes the CR.

### Permissions

Creating a `VirtualMachineImport` requires that you could create `DataVolumes`, `VirtualMachines` and
`VirtualMachineTemplates` in the target namespace yourself; a `ValidatingAdmissionPolicy` enforces
this. You do not need read access to the Secret named by `secretRef`.

## Development

```console
$ make all           # fmt, vet, vendor, lint, manifests, generate
$ make build         # bin/manager
$ make test          # unit and envtest suites
```

Against a live cluster:

```console
$ make cluster-up        # kubevirtci + KubeVirt + cert-manager
$ make cluster-sync      # build, push, deploy
$ make cluster-functest
$ make cluster-down
```

`CDI_VERSION` installs a specific CDI instead of the one bundled with kubevirtci - a release
tag (`v1.65.0`), a nightly date (`20260903`), or `nightly` for the most recent one:

```console
$ CDI_VERSION=nightly make cluster-up
```

See [AGENTS.md](AGENTS.md) for architecture, the module layout, and the reasoning behind how this
repo differs from [kubevirt/virt-template](https://github.com/kubevirt/virt-template), which it was
scaffolded from.

## Community

virt-import is part of the [KubeVirt](https://kubevirt.io) project, developed by SIG-compute.

## License

Apache License 2.0, see [LICENSE](LICENSE).
