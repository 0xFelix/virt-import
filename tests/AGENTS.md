# AGENTS.md - tests

Functional/integration tests (Ginkgo), run against a real cluster with KubeVirt, CDI and
virt-import deployed.

## Rules

- Run with `make cluster-functest` or `make kubevirt-functest`; always randomized
  (`-ginkgo.randomize-all`)
- `BeforeSuite` builds the fixtures of `internal/artifact/artifacttest` and pushes them over plain
  HTTP to `-push-registry` (the registry as the test host sees it, required), as
  `virt-import-tests/<fixture>:latest`. The make targets set it to the kubevirtci registry
- Settings are test flags, defined in `tests_suite_test.go`; `go test ./tests -args -help` lists
  them. `make functest` runs against any `KUBECONFIG`; pass `-push-registry` and `-registry` (the
  same registry as the cluster sees it, default `registry:5000`) in `FUNCTEST_EXTRA_ARGS`
- Requires `KUBECONFIG` - tests must skip cleanly when it's unset, not fail
- Every spec runs in its own `testNamespace`, deleted with everything in it after the spec; use
  `createNamespace` for more. Only clean up cluster-scoped objects yourself, with `DeferCleanup`
- Use `Eventually` with appropriately long timeouts (5 minutes is typical) for async operations
  (fetch Job, DataVolume import, export readiness, etc.)
- Disk contents are checked by checksum from a helper Pod (`diskChecksum`); imported VMs are not
  booted
- Run helper Pods through `runPod`; it applies the restricted Pod Security Standard and runs them
  as qemu (107), like KubeVirt's `libpod.RenderPod`
- Helper Pods use the image in `-test-image` (default
  `registry:5000/virt-import-functest:latest`), built from `tests/image/Dockerfile` and pushed by
  `cluster-sync` / `kubevirt-sync`. Do not pull test images from outside the cluster registry; add
  missing tools to `tests/image/Dockerfile` instead
- Specs labeled `KubeVirtExport` export with KubeVirt's OCI export and need a KubeVirt that
  annotates disk layers with the volume name; until a release does, they only pass on the
  `kubevirt-*` lane
- VMs are created from imported `VirtualMachineTemplate`s through virt-template's `create` and
  `process` subresources (`processTemplate`). Creating one in another namespace clones across
  namespaces and needs `allowCloneFrom`; it is the only spec proving the DataVolumeTemplate
  namespace rewrite, keep it
- Fixture disks must stay above 512 bytes compressed (CDI's importer reads a fixed 512 byte
  header) and template DataVolumeTemplates request at least 1Mi
- Prefer `DescribeTable` with `Entry` for parameterized test cases
