# virt-import

Import `VirtualMachines` and `VirtualMachineTemplates` from OCI artifacts stored in container
registries.

virt-import is the Phase 2 deliverable of
[VEP #395: OCI Artifact Import](https://github.com/kubevirt/enhancements/pull/396). It completes the
round trip started by [VEP #256](https://github.com/kubevirt/enhancements/pull/257) (OCI export):
export a VM on one cluster, push it to a registry, import it on another.

> **Status: alpha.** The import pipeline, the functional tests and the `virtimportctl` CLI are
> implemented. virt-import depends on CDI and KubeVirt changes that are not released yet, see
> [Requirements](#requirements). Targeting KubeVirt v1.10.

To try it out on a local cluster, follow the [demo](docs/demo.md): it exports a VM with two disks,
pushes it to a registry and imports it again.

## How it works

```
VirtualMachineImport CR
  |
  +-- 1. a metadata-fetch Job reads the artifact with the registry Secret and CA
  |       ConfigMap, resolves the platform, validates the artifact and reports
  |       the artifact type, config blob and disk layers
  |
  +-- 2. the controller rewrites the config blob so its volumes point at the
  |       PVCs to be imported, and keeps the result in a ConfigMap
  |
  +-- 3. one DataVolume per disk layer; CDI selects the layer by its
  |       io.kubevirt.disk.name annotation and streams the disk into a PVC
  |
  +-- 4. the VirtualMachine or VirtualMachineTemplate is created
  |
  +-- 5. the DataVolumes, and with them the PVCs, are handed over to the
          VirtualMachine or VirtualMachineTemplate, and the ConfigMap is deleted
```

The target kind is detected from the OCI manifest's `artifactType`. The controller never talks to a
registry itself: credentials stay in the metadata-fetch Job's Pod and in CDI's importer Pods, so the
controller needs no cluster-wide Secret access. The artifact is resolved once by the fetch Job and
every DataVolume imports from that exact digest, so a tag moved during the import cannot mix disks
of different versions.

Everything an import creates lands in the namespace of the `VirtualMachineImport`. Imports are
all-or-nothing; if any step fails, no target is created.

## Requirements

- KubeVirt. Artifacts must name each disk layer after its VM volume (`io.kubevirt.disk.name`), which
  KubeVirt's OCI export does once
  [kubevirt/kubevirt#19066](https://github.com/kubevirt/kubevirt/pull/19066) is merged. Exports from
  earlier KubeVirt versions cannot be imported.
- CDI with layer selection for registry DataVolumes
  ([kubevirt/containerized-data-importer#4301](https://github.com/kubevirt/containerized-data-importer/pull/4301),
  not released yet). With an older CDI, imports fail with "the installed CDI does not support layer
  selection for DataVolume registry sources".
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

To uninstall, delete all imports first, while the controller still runs to clean them up.
Otherwise their finalizers block the deletion of the CRD. Completed imports only lose their
`VirtualMachineImport`; the created VMs, templates and disks stay.

```console
$ kubectl delete virtualmachineimports --all --all-namespaces
$ kubectl delete -f https://github.com/kubevirt/virt-import/releases/latest/download/install.yaml
```

`make undeploy` does both.

virt-import runs two images: the controller (`quay.io/kubevirt/virt-import-controller`) and the
metadata fetcher (`quay.io/kubevirt/virt-import-fetcher`), which the controller starts as a Job for
every import.

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

- `targetName` defaults to the name of the `VirtualMachineImport`. Creating a target that already
  exists fails the import; an existing object is never overwritten.
- `secretRef` names a Secret with the keys `accessKeyId` and `secretKey`, the format CDI uses for
  registry DataVolumes.
- `certConfigMap` names a ConfigMap with the registry's CA certificates (`*.crt` or `*.pem` keys).
- Registries listed in CDI's `CDIConfig` `spec.insecureRegistries` are accessed without TLS
  verification, both by the fetch Job and by CDI.
- `storageClassName` applies to every imported disk. The PVCs are bound immediately, also on
  `WaitForFirstConsumer` storage classes.

Each disk becomes a PVC named `<targetName>-<hash>`, where the hash is derived from the target name,
the import and the disk name; `status.diskImports` lists which PVC holds which disk. Disks get the
size recorded in the artifact, but at least 1Mi, the smallest DataVolume CDI accepts.

For a multi-arch artifact, select the variant:

```yaml
spec:
  source:
    registry:
      url: docker://registry.example.com/templates/fedora:v1
      platform:
        architecture: arm64
```

Only the `linux` variants of a multi-arch artifact are considered, like CDI does. One with a
single such variant is used as-is when `architecture` is unset; one with several fails and asks
you to pick one. For a single-manifest artifact (for example one
copied without `--multi-arch=all`), a set `architecture` must match the one in its VM definition.

Watch progress:

```console
$ kubectl get vmimport
NAME        TARGET      AGE   READY   PROGRESSING   REASON
my-import   fedora-vm   2m    True    False         ImportComplete
```

The `REASON` column, the reason of the `Ready` condition, tells where an import is:

| Reason | Meaning |
|---|---|
| `Waiting` | CDI is not installed yet |
| `FetchingMetadata` | the fetch Job is reading the artifact |
| `ImportingDisks` | CDI is importing the disks |
| `CreatingTarget` | the disks are imported, the target is being created |
| `ImportComplete` | done, `status.targetRef` points at the created object |
| `ImportFailed` | the import failed for good; the message says why |

Temporary errors, such as an unavailable webhook, keep the current reason and show the error in the
`Ready` message while the controller retries. `status.artifactInfo` reports what was found in the
artifact, including the digest the disks are imported from, and `status.diskImports` tracks each
disk with its progress.

### virtimportctl

`virtimportctl` creates imports without writing YAML:

```console
$ virtimportctl create fedora --url docker://registry.example.com/vms/fedora:v1 \
    --secret registry-credentials --cert-configmap registry-ca --arch arm64
```

`create` also takes `--target-name` and `--storage-class`. Inspect and delete imports with
`kubectl`. `make build` builds it into `bin/virtimportctl`.

### VirtualMachineTemplates

A `VirtualMachineTemplate` artifact is imported the same way. Its `persistentVolumeClaim` volumes and
its `DataVolumeTemplates` cloning a PVC are pointed at the imported PVCs, including their namespace.
`DataVolumeTemplates` with other sources, such as a `DataSource`, are kept as they are.

VMs created from the template in the import's namespace work as-is. A VM created in another namespace
clones the imported PVCs across namespaces, which KubeVirt only allows if the VM's ServiceAccount
(`default` unless the VM sets one) may `create` `datavolumes/source` in the import's namespace:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: clone-imported-disks
  namespace: <import namespace>
rules:
- apiGroups: ["cdi.kubevirt.io"]
  resources: ["datavolumes/source"]
  verbs: ["create"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: clone-imported-disks
  namespace: <import namespace>
subjects:
- kind: ServiceAccount
  name: default
  namespace: <VM namespace>
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: clone-imported-disks
```

### Cleanup

Deleting a `VirtualMachineImport` **before** it completes removes the intermediate resources,
including the DataVolumes and PVCs. Once an import completes, the DataVolumes and their PVCs are
owned by the created VM or template and are deleted with it, like the disks of a VM created with
`dataVolumeTemplates`. Deleting the `VirtualMachineImport` only removes the CR.

### Permissions

Creating a `VirtualMachineImport` requires that you could create `DataVolumes`, `VirtualMachines` and
`VirtualMachineTemplates` in the target namespace yourself; a `ValidatingAdmissionPolicy` enforces
this. You do not need read access to the Secret named by `secretRef`.

## Known limitations

- The metadata fetch does not use CDI's import proxy (`importProxy` in `CDIConfig`) yet. On
  clusters that need a proxy to reach the registry, imports fail while fetching the metadata.

## Development

```console
$ make all           # fmt, vet, vendor, lint, manifests, generate
$ make build         # bin/manager, bin/fetcher, bin/virtimportctl
$ make test          # unit and envtest suites
```

Against a live cluster:

```console
$ make cluster-up        # kubevirtci + KubeVirt + CDI + cert-manager
$ make cluster-sync      # build, push, deploy
$ make cluster-functest
$ make cluster-down
```

The functional tests build their test artifacts in Go, push them to the cluster's registry and
import them. To run them against another cluster with `make functest`, pass `-push-registry` (a
registry served over plain HTTP as your machine reaches it), `-registry` (the same registry as the
cluster reaches it) and `-test-image` (the helper image built from `tests/image/Dockerfile`) in
`FUNCTEST_EXTRA_ARGS`.

`kubevirt-up`, `kubevirt-sync`, `kubevirt-functest` and `kubevirt-down` do the same with KubeVirt
built from `KUBEVIRT_REPO` at `KUBEVIRT_REF` (a branch, tag or commit). The checkout in `_kubevirt`
is reused; remove it after `make kubevirt-down` to build another ref or newer commits of the same
branch. Likewise, remove `_cluster-up` after `make cluster-down` to move to a newer KubeVirt release.

Both lanes enable KubeVirt's OCI export for the round-trip tests, which export a VM and a template
and import them again. They need KubeVirt changes that are not released yet, so for now they only
pass on the `kubevirt-*` lane.

`CDI_VERSION` picks the CDI to install - a release tag (`v1.65.0`), a nightly date (`20260903`),
`nightly` for the most recent one, or `source` to build it from `CDI_REPO` at `CDI_REF`:

```console
$ CDI_VERSION=nightly make cluster-up
```

Until the KubeVirt and CDI changes listed under [Requirements](#requirements) are released, the
Makefile defaults to building CDI from source at the commit virt-import is built against, and to a
KubeVirt commit carrying the export changes.

## Community

virt-import is part of the [KubeVirt](https://kubevirt.io) project, developed by SIG-compute.

## License

Apache License 2.0, see [LICENSE](LICENSE).
