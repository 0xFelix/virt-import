# AGENTS.md - api/core

Public API types for the `import.kubevirt.io` group (module `kubevirt.io/virt-import-api`).

- `v1alpha1/` - `VirtualMachineImport` CRD type definitions

## Rules

- Treat any change to `VirtualMachineImport` fields as a potentially breaking API change - call it out
  explicitly in the PR description, and name deviations from [VEP #395](https://github.com/kubevirt/enhancements/pull/396)
- Every exported field requires a kubebuilder validation marker (where applicable) and a godoc comment
- If you modify these type definitions, always run `make generate` (DeepCopy, typed client) and
  `make manifests` (CRDs) afterward, and commit the regenerated output
- Spec immutability (`self == oldSelf`), the required registry source, its `docker://` URL and valid
  object names are CEL rules declared on the API types - do not remove these markers, the
  controller relies on them
