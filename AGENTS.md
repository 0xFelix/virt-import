# AGENTS.md - virt-import

## Strict Rules

- Never modify generated files by hand - use `make generate`, `make manifests`, and `make vendor`
- Never hand-edit generated manifests under `config/` (CRDs, RBAC) - regenerate with `make manifests`
  instead
- Never remove or modify Apache 2.0 license headers (see `hack/boilerplate.go.txt`)
- Never bypass linting or skip `make all` before pushing
- Never commit vendor changes without running `make vendor`
- Never modify CRD type definitions without running `make generate` and `make manifests` afterward
- Treat changes to `VirtualMachineImport` fields in `api/core/` as potentially breaking the API - all
  exported fields require kubebuilder validation markers and godoc comments

Several directories also carry their own nested `AGENTS.md` with directory-specific conventions (see
"Key directories" below) - read the closest one for the file(s) you're touching, in addition to this
file.

## Project Overview

virt-import imports `VirtualMachines` and `VirtualMachineTemplates` from OCI artifacts stored in
container registries, completing the round trip started by OCI export (VEP #256). It is the Phase 2
deliverable of [VEP #395](https://github.com/kubevirt/enhancements/pull/396).

The target kind is auto-detected from the OCI manifest's `artifactType`. Disk data is not fetched by
this controller: it is delegated to CDI via `DataVolumes`.

**Current state: Alpha.** The import pipeline (metadata-fetch Job, DataVolume creation, config blob
rewriting, target creation, cleanup), functional tests including export/import round trips, and the
`virtimportctl` CLI are implemented. It builds on unmerged CDI, KubeVirt and VEP changes; see
"Temporary solutions" below before touching anything they affect.

## Architecture

### Multi-module Go workspace

`go.work` ties three modules together:

| Module | Path | Purpose |
|---|---|---|
| `kubevirt.io/virt-import` | `.` | controller manager |
| `kubevirt.io/virt-import-api` | `./api` | API types, published standalone |
| `kubevirt.io/virt-import-client-go` | `./staging/src/kubevirt.io/virt-import-client-go` | generated clientset, published standalone |

`tools/` is a fourth module deliberately **outside** the workspace. It pins build tools via Go `tool`
directives and is invoked with `GOWORK=off`. Never `go install` a tool; add it to `tools/go.mod` and
call it through the `go-tool` macro in the Makefile.

`api/` and `staging/` are pushed to the standalone `kubevirt/virt-import-{api,client-go}` GitHub repos
by `hack/publish-staging.sh`, which the nightly `periodic-virt-import-publish-staging-main-nightly`
job runs for `main` and `hack/release.sh` runs for release tags.

### Key directories

```
api/core/v1alpha1/          VirtualMachineImport types (import.kubevirt.io/v1alpha1) - see api/core/AGENTS.md
cmd/main.go                 controller manager entrypoint
cmd/fetcher/                metadata-fetch Job entrypoint, prints one {"fetchResult":...} line on stdout
cmd/virtimportctl/          the virtimportctl CLI (create)
internal/controller/        reconciler + CRD availability gating - see internal/controller/AGENTS.md
internal/artifact/          OCI artifact format (VEP #256): validation, platform selection, fetch result, config rewrite, fetcher flags and exit codes
  artifacttest/             builds VEP #256 artifacts and the fixtures for tests
internal/fetcher/           reads and validates artifact metadata with containers/image (go.podman.io/image/v5)
internal/virtimportctl/     virtimportctl, built from virtctl's modules (see below)
internal/scheme/            runtime scheme registration
internal/apimachinery/      GetStableName, DNS-1035-safe deterministic child names
internal/logs/              log level constants
config/                     Kustomize manifests, two overlays - see config/AGENTS.md
tests/                      functional tests (need KUBECONFIG) - see tests/AGENTS.md
```

### Dependency on virt-template

virt-import imports `kubevirt.io/virt-template-api` for the `VirtualMachineTemplate` type, because a
`VirtualMachineTemplate` is one of the two possible import targets. This is the one place where
`template.kubevirt.io` legitimately appears in this repo - in `internal/artifact/config.go`, in the
RBAC marker on the reconciler, and in the admission policy. Do not "fix" those to
`import.kubevirt.io`.

### Temporary CDI API

DataVolume layer selection (`spec.source.registry.layer`) comes from
[CDI #4301](https://github.com/kubevirt/containerized-data-importer/pull/4301), merged but not
released. Until a CDI release contains it, `go.mod` replaces `kubevirt.io/containerized-data-importer-api`
with its merge commit, and the Makefile's `CDI_VERSION` default deploys the CDI nightly built from
the same commit. Re-pin both together and refresh the envtest CRDs with `make cluster-envtest-crds` or
`make kubevirt-envtest-crds`; once released, drop the `replace` and the CDI defaults and bump to
the release tag.

### virtimportctl

`internal/virtimportctl` is laid out like virtctl in kubevirt/kubevirt (`pkg/virtctl`), so it can
move into `virtctl vmimport` later. `clientconfig/`, `templates/` and `testing/` are copies of
virtctl's modules and must stay in sync with them: change them only where virt-import needs to
(import paths, names). `root.go` is virtctl's `root.go` with virt-import's names and commands.

`kubevirt.io/client-go/kubecli` cannot be imported while it does not support Kubernetes 0.37, so
`internal/virtimportctl/kubecli` holds copies of the few kubecli functions the virtctl modules use.
Replace its imports with the real kubecli once it does, and delete it. `create` gets its import
client through `GetImportClient`, like virtctl's `template create`.

## Controller Design

### VirtualMachineImport controller

`internal/controller/virtualmachineimport_controller.go`. Owns the metadata-fetch `Job`, the manifest
`ConfigMap` and the `DataVolumes` it creates, and maps the fetch Pods back by label. See
[internal/controller/AGENTS.md](internal/controller/AGENTS.md) for the pipeline and its invariants.

The manager reads the fetcher image from the required `FETCHER_IMAGE` env var. The Makefile
sets it through a local-config ConfigMap and a kustomize replacement in `config/manager`, since the
images transformer only touches container images.

### CRD availability gating

`internal/controller/availability_controller.go` is the non-obvious piece, ported from
virt-template's `vmtr_availability_controller.go`.

`SetupWithManager` builds an informer per watched type, and an informer for an uninstalled CRD fails
the manager. CDI is an optional KubeVirt add-on, so the DataVolume watch cannot be registered
unconditionally. `AvailabilityController` is a `manager.Runnable` that:

1. polls discovery for every resource in `requiredResources` (CDI `datavolumes` and `cdiconfigs`),
2. calls `SetupWithManager` on the reconciler only once they are all present,
3. meanwhile patches `Ready=False/Reason=Waiting` + `Progressing=True` onto pending
   `VirtualMachineImports`, so users see why nothing is happening instead of a crashlooping pod.

`ExternalCRDCacheConfig(dc)` is its companion, consumed by `cmd/main.go` when building the manager.
It label-scopes the Job, Pod, ConfigMap and DataVolume informers to `import.kubevirt.io/ImportUID`
so the cache holds only children this controller created, field-selects the single CDI `CDIConfig`
named `config`. Targets are unstructured, which the client never caches,
so the optional `template.kubevirt.io` group needs no cache entry. The CDIConfig informer is
started by `startController` because nothing watches it.

**If you add an external watch or read**, extend `requiredResources` and `ExternalCRDCacheConfig` - do not
call `SetupWithManager` directly from `cmd/main.go`.

### Authorization

There is no Go webhook. Spec immutability, a required registry source, its `docker://` URL and valid
object names are CRD CEL rules (`+kubebuilder:validation:XValidation`), and privilege-escalation is prevented
by a `ValidatingAdmissionPolicy` in `config/admission/`: creating a `VirtualMachineImport` requires
that the user could create `DataVolumes`, `VirtualMachines` and `VirtualMachineTemplates` in the
target namespace themselves. Both possible targets are checked up front, since the artifact has not
been inspected at admission time.

## Deliberate omissions from virt-template

virt-import was scaffolded from `kubevirt/virt-template`. Three things were dropped on purpose:

- **Aggregated API server.** virt-template serves `process`/`create` subresources through one.
  virt-import has no subresources. Restore from virt-template's `cmd/apiserver/`,
  `internal/apiserver/`, `config/apiserver/` and `apiserver.Dockerfile` if ever needed.
- **Validating webhooks.** See Authorization above. To add one back, restore
  `config/webhook/`, `config/certmanager/certificate-webhook.yaml`,
  `config/components/deployment-patches/manager_webhook_patch.yaml`, and the four
  `webhook-service`/`serving-cert` replacement blocks in `config/default/kustomization.yaml`.
- **The template engine module and `openapi-gen`.** The engine is template-processing logic with no
  analogue here. `openapi-gen` only ever fed the aggregated API server's OpenAPI spec.

cert-manager was **kept**, issuing the metrics serving certificate. `--metrics-cert-path` is
technically optional (controller-runtime self-signs when it is unset), but the fallback certificate
has SANs for `localhost` only, which would force `insecureSkipVerify` on any scraper reaching the
Service by DNS name.

## Build and Development

### Prerequisites

Go, podman or docker, kubectl, and jq to pull the envtest CRDs (`make *-envtest-crds`). Build
tools come from `tools/go.mod`, not your `$PATH`.

### Key make targets

| Target | Purpose |
|---|---|
| `make all` | fmt, vet, vendor, lint, manifests, generate, check-uncommitted |
| `make build` | build `bin/manager`, `bin/fetcher` and `bin/virtimportctl` |
| `make container-build` | controller and fetcher images (`container-build-controller`, `container-build-fetcher`) |
| `make test` | unit + envtest suites |
| `make manifests` | CRDs and RBAC via controller-gen |
| `make generate` | deepcopy via controller-gen, clientset via `hack/generate.sh` |
| `make vendor` | tidy all modules, `go work vendor`, full module graph checksums in `go.work.sum`, vendor `tools/` |
| `make build-installer` | `dist/install.yaml` (standalone, cert-manager) |
| `make build-installer-hco` | `dist/install-hco.yaml`, the bundle HCO embeds |

Run `make all` before pushing. It ends in `check-uncommitted`, which fails if generation produced a
diff.

### Cluster development

```
make cluster-up        # kubevirtci + released KubeVirt + cert-manager
make cluster-sync      # build, push to registry:5000, redeploy
make cluster-functest  # functional tests against that cluster
make cluster-down
```

`make undeploy`, which `*-sync` runs first, deletes all imports while the controller still runs;
otherwise their finalizers would block the deletion of the CRD.

`cluster-up` clones kubevirtci at the tag the latest KubeVirt release uses into `_cluster-up`. An
existing `_cluster-up` is reused as it is; remove it after `cluster-down` to move to a newer release.

`kubevirt-up` / `-sync` / `-functest` / `-down` do the same against KubeVirt built from git:
`KUBEVIRT_TAG` (a branch or tag, `main` by default), cloned into `_kubevirt`. An existing
`_kubevirt` is reused as it is; remove it after `kubevirt-down` to switch or pick up new commits.
Both `up` paths enable the `OCIExport` feature gate for the functional tests importing KubeVirt
exports.

Rebuilding KubeVirt on a running cluster (`make cluster-sync` inside `_kubevirt`) deletes and
recreates the `kubevirt` namespace, which removes virt-import, and resets the feature gates.
Re-enable `OCIExport` and run `make kubevirt-sync` afterwards.

Without `CDI_VERSION` both `up` paths install the CDI bundled with kubevirtci. Set it to install a
different one instead - useful while virt-import depends on CDI features that are not yet in a
kubevirtci release:

```
CDI_VERSION=v1.65.0  make cluster-up   # a release tag, from the GitHub release assets
CDI_VERSION=20260903 make cluster-up   # a nightly date, from the kubevirt-prow bucket
CDI_VERSION=nightly  make cluster-up   # the most recent nightly
CDI_VERSION=source   make cluster-up   # built from CDI_REPO at CDI_REF, pushed to the cluster registry
```

**Temporary default.** The Makefile defaults `CDI_VERSION` to the nightly built from the CDI commit
`go.mod` replaces the CDI API with. Re-pin it together with the `go.mod` replace; drop the default
once #4301 is released.

`hack/cdi.sh` resolves the value to a manifest base (failing before cluster-up if it is
unusable; for `source` the manifests CDI's `make manifests` renders into `_out/cdi-src`), turns off `KUBEVIRT_DEPLOY_CDI` so kubevirtci does not install a CDI we are about to
replace, applies `cdi-operator.yaml` and `cdi-cr.yaml`, and waits for the CDI CR. It also owns
the `insecureRegistries: [registry:5000]` patch for both paths - KubeVirt's `cluster-deploy.sh`
applies that patch only when it deploys CDI itself.

## Linting

Run `make lint` to execute all linters:

- **golangci-lint** (version in `tools/go.mod`) with `.golangci.yml` - line length 140, cyclomatic
  complexity 15, function length 100 lines / 50 statements
- **hack/lint.sh** - runs `yamllint` on `config/` and `shellcheck` on `hack/*.sh` (both must be
  installed on the host)
- **hack/license-header-check.sh** - Apache 2.0 header (see `hack/boilerplate.go.txt`)
- **gofumpt** - formatting (stricter than gofmt)

## Testing

Ginkgo v2 + Gomega throughout, `envtest` for controller suites.

- Unit/envtest: `make test`. `config/crd/testing/` holds third-party CRDs (CDI `DataVolume` and
  `CDIConfig`, virt-template `VirtualMachineTemplate`, KubeVirt `VirtualMachine`) that envtest needs.
  `make cluster-envtest-crds` or `make kubevirt-envtest-crds` pulls them from a running cluster; do
  not edit them by hand.
- Functional: `make functest`, skipped when `KUBECONFIG` is unset. See [tests/AGENTS.md](tests/AGENTS.md).
- `internal/controller/availability_controller_test.go` is the suite worth reading first: it starts an
  environment with the external CRDs deliberately absent and asserts the manager waits rather than
  crashes.

## Deployment

Kustomize for both the standalone install and the bundle HCO deploys. No Helm, no OLM bundle.

- `config/default` - cert-manager issues the metrics cert. Used by `make deploy` and
  `dist/install.yaml`.
- `config/hco` - the HCO bundle: CRD, RBAC, aggregated roles, manager, admission policy, metrics
  Service, ServiceMonitor, metrics ingress network policy and a metrics token Secret, no
  cert-manager or egress network policies. Rendered by `make build-installer-hco` into
  `dist/install-hco.yaml`, a release asset like `install.yaml`.

### HCO bundle

HCO embeds `install-hco.yaml`, synced from the release matching its pinned version and checked
against `CHECKSUMS.sha256`, and deploys everything in it at runtime when its `deployVirtImport`
feature gate is enabled, the CRD included (same pattern as virt-template in virt-operator). It moves
all objects to its namespace, adds its labels and node placement, and sets the manager image and
`FETCHER_IMAGE` from its own environment, so the image references in the bundle are informational.
Disabling the feature gate deletes the CRD first and the rest once the CRD is gone, so the controller
can still remove the finalizers.

Contract with HCO, which rejects a bundle that breaks it:

- kinds: CRD, ServiceAccount, (Cluster)Role, (Cluster)RoleBinding, Deployment,
  ValidatingAdmissionPolicy, ValidatingAdmissionPolicyBinding, Service, Secret, ServiceMonitor and
  NetworkPolicy
- exactly one Deployment, with exactly one container named `manager`, which has a `FETCHER_IMAGE`
  env var, and a `metrics-certs` volume from a Secret
- exactly one Service, and exactly one ServiceMonitor selecting it, with one endpoint and no
  `tlsConfig`, `authorization` or `bearerTokenFile`
- at most one Secret, of type `kubernetes.io/service-account-token`, without data
- NetworkPolicies only for ingress, selecting the manager pods
- RBAC changes need the same rules in HCO's ClusterRole, a unit test in HCO enforces it

HCO injects what depends on the cluster:

- the serving certificate in a Secret named `virt-import-metrics-server-cert`, mounted as the
  `metrics-certs` volume, from service-ca on OpenShift and from cert-manager on Kubernetes
- `--tls-min-version`, `--tls-cipher-suites` and `--tls-curve-preferences` from its TLS profile
- on OpenShift `--metrics-client-ca-configmap` and `--metrics-allowed-client-cn`, so Prometheus
  authenticates with its client certificate without calls to the API server; elsewhere Prometheus
  uses the token in the `metrics-token` Secret
- the ServiceMonitor namespace selector and endpoint TLS and auth

HCO builds the network policy that allows egress to DNS and the API server.

## Temporary solutions

Each item below works around something unmerged or unreleased. Resolve it when its trigger happens.

| Item | Workaround | Trigger and action |
|---|---|---|
| CDI #4301 (layer selection) | `go.mod` `replace` of the CDI API to the merge commit, `CDI_VERSION` default deploys the nightly built from it (see "Temporary CDI API") | CDI release: drop the `replace` and the CDI defaults, bump to the tag, refresh the envtest CRDs with `make cluster-envtest-crds`, name the minimum CDI version in README.md |
| CDI in e2e | Makefile defaults `CDI_VERSION` to the nightly of the `replace` commit | Release: pin the tag |
| enhancements #464 (persistent state, relaxed validation) | Strict validation: every PVC volume needs a disk layer and every layer a volume, only `disk.raw+zstd` layers | Merged: allow PVC volumes without layer (left untouched), skip one persistent state layer, update the fixtures |
| OCI constants | Copied from KubeVirt's unexported `pkg/storage/oci` into `internal/artifact/constants.go` | Exported upstream (e.g. in `kubevirt.io/api`): import them |
| kubecli and Kubernetes 0.37 | `internal/virtimportctl/kubecli` stand-in, generated clientsets in `tests/` | kubecli supports 0.37: use it, delete the stand-in |

The VEP itself still describes some behavior differently (DV/PVC naming, digest pinning, fetcher
output contract, cleanup timing, DataVolumes owned by the target); the code is authoritative until the VEP is updated.

## Conventions

- API group: `import.kubevirt.io`, version `v1alpha1`
- All source files carry Apache 2.0 license headers from `hack/boilerplate.go.txt`
- Commit messages: conventional commits with scope, e.g. `feat(config,admission): ...`
- Commits must be signed off (`git commit -s`)
- PRs and issues must follow the GitHub templates in `.github/`. Always read the template before
  creating a PR or issue and fill in all sections.
- Container images: `quay.io/kubevirt/virt-import-controller` (`Dockerfile`) and
  `quay.io/kubevirt/virt-import-fetcher` (`fetcher.Dockerfile`, runs in the metadata-fetch Job).
  `tests/image/Dockerfile` builds `virt-import-functest`, the helper Pod image of the functional
  tests; only `cluster-sync` / `kubevirt-sync` build it and push it to the kubevirtci registry, it
  is never released or pushed elsewhere
- Multi-arch: linux/amd64, linux/arm64, linux/s390x
- Generated client code lives in `staging/`
- Logging levels: `V(1)` for debug, `V(2)` for trace
