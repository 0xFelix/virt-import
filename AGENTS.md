# AGENTS.md - virt-import

## Strict Rules

- Never modify generated files by hand - use `make generate`, `make manifests`, and `make vendor`
- Never remove or modify Apache 2.0 license headers (see `hack/boilerplate.go.txt`)
- Never bypass linting or skip `make all` before pushing
- Never commit vendor changes without running `make vendor`
- Never modify CRD type definitions without running `make generate` and `make manifests` afterward

## Project Overview

virt-import imports `VirtualMachines` and `VirtualMachineTemplates` from OCI artifacts stored in
container registries, completing the round trip started by OCI export (VEP #256). It is the Phase 2
deliverable of [VEP #395](https://github.com/kubevirt/enhancements/pull/396).

The target kind is auto-detected from the OCI manifest's `artifactType`. Disk data is not fetched by
this controller: it is delegated to CDI via `DataVolumes`.

**Current state: scaffolding.** The `VirtualMachineImport` API and the controller wiring exist;
`Reconcile` is a no-op. The import state machine (metadata-fetch Job, DataVolume creation, config
blob rewriting, target creation, finalizer GC) is not implemented yet.

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

`api/` and `staging/` are pushed to standalone `kubevirt/virt-import-{api,client-go}` GitHub repos by
`hack/publish-staging.sh`. Those repos do not exist yet, so that script and `hack/release.sh` cannot
run until they are created.

### Key directories

```
api/core/v1alpha1/          VirtualMachineImport types (import.kubevirt.io/v1alpha1)
cmd/main.go                 controller manager entrypoint
cmd/fetcher/                metadata-fetch Job entrypoint (placeholder, no-op for now)
internal/controller/        reconciler + CRD availability gating
internal/artifact/          OCI artifact format (VEP #256): validation, platform selection, fetch result, config rewrite
internal/testing/artifacts/ builds VEP #256 artifacts for tests, and the fixtures
internal/scheme/            runtime scheme registration
internal/apimachinery/      GetStableName, DNS-1035-safe deterministic child names
internal/logs/              log level constants
config/                     Kustomize manifests, two overlays
tests/                      functional tests (need KUBECONFIG)
tests/artifacts/            generated oci-archive fixtures (`make generate`), generator in `generate/`
```

### Dependency on virt-template

virt-import imports `kubevirt.io/virt-template-api` for the `VirtualMachineTemplate` type, because a
`VirtualMachineTemplate` is one of the two possible import targets. This is the one place where
`template.kubevirt.io` legitimately appears in this repo - in `internal/scheme/scheme.go`, in the
RBAC marker on the reconciler, and in the admission policy. Do not "fix" those to
`import.kubevirt.io`.

### Temporary CDI API

DataVolume layer selection (`spec.source.registry.layer`) comes from the unmerged
[CDI #4301](https://github.com/kubevirt/containerized-data-importer/pull/4301). Until a CDI release
contains it, `go.mod` replaces `kubevirt.io/containerized-data-importer-api` with that PR's commit on
the author's fork, and `config/crd/testing/cdi.kubevirt.io_datavolumes.yaml` is the `datavolume` CRD
extracted from `pkg/operator/resources/crds_generated.go` at the same commit. Re-pin both together;
once released, drop the `replace`, bump to the release tag and re-extract the CRD from it.

## Controller Design

### VirtualMachineImport controller

`internal/controller/virtualmachineimport_controller.go`. Watches `VirtualMachineImport` and owns the
`DataVolumes` it creates (`Owns(&cdiv1beta1.DataVolume{})`). `Reconcile` is currently a no-op.

### CRD availability gating

`internal/controller/availability_controller.go` is the non-obvious piece, ported from
virt-template's `vmtr_availability_controller.go`.

`SetupWithManager` builds an informer per watched type, and an informer for an uninstalled CRD fails
the manager. CDI is an optional KubeVirt add-on, so the DataVolume watch cannot be registered
unconditionally. `AvailabilityController` is a `manager.Runnable` that:

1. polls discovery for everything in `requiredGroups` (currently just `cdi.kubevirt.io/v1beta1`),
2. calls `SetupWithManager` on the reconciler only once they are all present,
3. meanwhile patches `Ready=False/Reason=Waiting` + `Progressing=True` onto pending
   `VirtualMachineImports`, so users see why nothing is happening instead of a crashlooping pod.

`ExternalCRDCacheConfig(dc)` is its companion, consumed by `cmd/main.go` when building the manager.
It label-scopes the DataVolume informer to `import.kubevirt.io/ImportUID` so the cache holds only
DataVolumes this controller created, and puts `VirtualMachineTemplate` in `clientDisableFor` because
it is a create-only target that must not start an informer for a possibly-absent CRD.

**If you add a second external watch**, extend `requiredGroups` and `ExternalCRDCacheConfig` - do not
call `SetupWithManager` directly from `cmd/main.go`.

### Authorization

There is no Go webhook. Spec immutability is a CRD CEL rule
(`+kubebuilder:validation:XValidation:rule="self == oldSelf"`), and privilege-escalation is prevented
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

Go, podman or docker, kubectl. Build tools come from `tools/go.mod`, not your `$PATH`.

### Key make targets

| Target | Purpose |
|---|---|
| `make all` | fmt, vet, vendor, lint, manifests, generate, check-uncommitted |
| `make build` | build `bin/manager` and `bin/fetcher` |
| `make container-build` | controller and fetcher images (`container-build-controller`, `container-build-fetcher`) |
| `make test` | unit + envtest suites |
| `make manifests` | CRDs and RBAC via controller-gen |
| `make generate` | deepcopy via controller-gen, clientset via `hack/generate.sh`, test artifact fixtures |
| `make vendor` | tidy all modules, `go work vendor`, vendor `tools/` |
| `make build-installer` | `dist/install.yaml` (standalone, cert-manager) |
| `make build-csv-manifests` | `_out/manifests.yaml`, the CSV input baked into the image |
| `make csv` | print the ClusterServiceVersion, as HCO gets it |

Run `make all` before pushing. It ends in `check-uncommitted`, which fails if generation produced a
diff.

### Cluster development

```
make cluster-up        # kubevirtci + released KubeVirt + cert-manager
make cluster-sync      # build, push to registry:5000, redeploy
make cluster-functest  # functional tests against that cluster
make cluster-down
```

`kubevirt-up` / `-sync` / `-functest` / `-down` do the same against KubeVirt built from git.

Both `up` paths install the CDI bundled with kubevirtci. Set `CDI_VERSION` to install a
different one instead - useful while virt-import depends on CDI features that are merged but
not yet in a kubevirtci release:

```
CDI_VERSION=v1.65.0  make cluster-up   # a release tag, from the GitHub release assets
CDI_VERSION=20260903 make cluster-up   # a nightly date, from the kubevirt-prow bucket
CDI_VERSION=nightly  make cluster-up   # the most recent nightly
```

`hack/cdi.sh` resolves the value to a manifest base URL (failing before cluster-up if it is
unusable), turns off `KUBEVIRT_DEPLOY_CDI` so kubevirtci does not install a CDI we are about to
replace, applies `cdi-operator.yaml` and `cdi-cr.yaml`, and waits for the CDI CR. It also owns
the `insecureRegistries: [registry:5000]` patch for both paths - KubeVirt's `cluster-deploy.sh`
applies that patch only when it deploys CDI itself.

## Testing

Ginkgo v2 + Gomega throughout, `envtest` for controller suites.

- Unit/envtest: `make test`. `config/crd/testing/` holds third-party CRDs (CDI `DataVolume`,
  virt-template `VirtualMachineTemplate`) that envtest needs.
- Functional: `make functest`, skipped when `KUBECONFIG` is unset.
- `internal/controller/availability_controller_test.go` is the suite worth reading first: it starts an
  environment with the external CRDs deliberately absent and asserts the manager waits rather than
  crashes.

## Deployment

Kustomize for the standalone install, a generated `ClusterServiceVersion` for OLM. No Helm, no
committed bundle.

- `config/default` - cert-manager issues the metrics cert. Used by `make deploy` and
  `dist/install.yaml`.
- `config/csv` - the CSV base: CRD, RBAC, manager and network policies, no metrics or cert-manager.

### csv-generator

HCO runs the controller image with `--entrypoint=/csv-generator` (via the
`org.kubevirt.hco.csv-generator.v1` label) and consumes its stdout; `make csv` prints it locally.

`cmd/csv-generator` reads `/data/manifests.yaml` - the `config/csv` render baked in by
`make build-csv-manifests` - and hoists the Deployment into the install strategy and the RBAC into
`clusterPermissions`/`permissions` per service account, with metadata from the embedded
`csv-base.yaml`. operator-sdk is unused: it cannot be a `go tool` (containers/image needs cgo and
gpgme, `go tool` passes no build tags) and HCO discards the bundle scaffolding anyway.

HCO keeps only the CSV and CRDs, and a CSV carries just Deployments and RBAC, so an HCO install gets
no metrics Service, ServiceMonitor, NetworkPolicies or ValidatingAdmissionPolicy. Hence no metrics in
the CSV: the manager leaves `--metrics-bind-address` at `0`, needing no cert and no cert-manager.

## Conventions

- API group: `import.kubevirt.io`, version `v1alpha1`
- All source files carry Apache 2.0 license headers from `hack/boilerplate.go.txt`
- Commit messages: conventional commits with scope, e.g. `feat(config,admission): ...`
- Commits must be signed off (`git commit -s`)
- PRs and issues must follow the GitHub templates in `.github/`. Always read the template before
  creating a PR or issue and fill in all sections.
- Container images: `quay.io/kubevirt/virt-import-controller` (`Dockerfile`) and
  `quay.io/kubevirt/virt-import-fetcher` (`fetcher.Dockerfile`, runs in the metadata-fetch Job)
- Multi-arch: linux/amd64, linux/arm64, linux/s390x
- Generated client code lives in `staging/`
- Logging levels: `V(1)` for debug, `V(2)` for trace
