# AGENTS.md - config

Kubebuilder-based Kustomize manifests for deploying virt-import.

## Rules

- To change generated manifests here (CRDs, RBAC), update the source markers in `api/` type
  definitions or the `+kubebuilder:rbac` markers in the code and run `make manifests` to regenerate
- Always keep YAML in this directory lint-clean (`hack/lint.sh` runs `yamllint` against it)
- `crd/testing/` holds third-party CRDs for envtest, pulled from a cluster by
  `make cluster-envtest-crds` or `make kubevirt-envtest-crds` - do not edit them by hand
- `admission/` holds the `ValidatingAdmissionPolicy` that prevents privilege escalation, see
  "Authorization" in the root `AGENTS.md`

## Overlays

- `default` - standalone Kubernetes with cert-manager
- `hco` - the bundle the hyperconverged-cluster-operator embeds, see "HCO bundle" in the root
  `AGENTS.md`
