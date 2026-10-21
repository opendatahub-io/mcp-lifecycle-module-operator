# Contributing

## Prerequisites

- Go 1.26+
- [Kind](https://kind.sigs.k8s.io/) (for local E2E testing)
- Podman or Docker
- `kubectl`

Build tools (`kustomize`, `controller-gen`) are downloaded automatically into `./bin/` by the Makefile. Container targets prefer Podman when installed, otherwise Docker. Override with `CONTAINER_TOOL=docker` or `CONTAINER_TOOL=podman`.

## Make Targets

Run `make help` for the full list. Key targets:

| Target | Description |
|---|---|
| `make build` | Build the manager binary (runs codegen + fmt + vet first) |
| `make test` | Run unit tests with coverage (runs codegen first) |
| `make unit-test` | Run unit tests only (no codegen prerequisites) |
| `make verify` | Check local codegen, formatting, dependencies, and updater unit tests |
| `make verify-operand-manifests` | Regenerate and compare operand outputs from saved source/image pins (network required) |
| `make manifests` | Generate CRD and RBAC YAML from kubebuilder markers |
| `make generate` | Generate DeepCopy implementations |
| `make docker-build` | Build the container image |
| `make install` | Install CRDs into the current cluster |
| `make deploy` | Deploy the full operator to the current cluster |
| `make update-operand-manifests` | Re-vendor operand manifests from the midstream fork |
| `make kind-create` | Create a Kind cluster with a local container registry |
| `make e2e-test` | Run E2E tests against a running cluster |

## Development Workflow

### Running locally

```bash
# Install CRDs into your cluster
make install

# Run the controller on your host (connects to current kubeconfig)
make run
```

The controller requires two environment variables: `SYSTEM_NAMESPACE` and `OPERATOR_VERSION`. When running locally, export them:

```bash
export SYSTEM_NAMESPACE=mcp-lifecycle-module-operator-system
export OPERATOR_VERSION=dev
make run
```

### Running on a Kind cluster

```bash
# Create a Kind cluster with a local registry on localhost:5001
make kind-create

# Build and push to the local registry
make docker-build docker-push IMAGE_REGISTRY=localhost:5001 IMAGE_TAG=dev

# Deploy
make deploy IMAGE_REGISTRY=localhost:5001 IMAGE_TAG=dev

# Apply the sample CR
kubectl apply -f config/samples/mcplifecycleoperator.yaml
```

`make deploy` uses the operand digest embedded in the installer by default and clears any previous deployment override. To use a mirrored or custom operand image, pass `MCPLO_OPERAND_IMAGE=<image-reference>`. Platform-provided `RELATED_IMAGE_ODH_MCP_LIFECYCLE_OPERATOR_IMAGE` overrides remain supported at runtime.

## Code Generation

This project uses [kubebuilder](https://book.kubebuilder.io/) markers for code generation:

- **CRD + RBAC manifests**: generated from `// +kubebuilder:rbac:...` and type annotations. Run `make manifests` after changing markers.
- **DeepCopy methods**: generated from types in `api/`. Run `make generate` after changing API types.

Always run `make manifests generate` before committing changes to API types or RBAC markers.

## Updating Operand Manifests

The operand (MCP Lifecycle Operator) manifests are vendored at `internal/controller/resources/mcp-lifecycle-operator.yaml`. They are obtained by running `kustomize build` on the midstream fork at [opendatahub-io/mcp-lifecycle-operator](https://github.com/opendatahub-io/mcp-lifecycle-operator).

To update them:

```bash
# Use main or the current release branch automatically
make update-operand-manifests

# Override with a specific branch, tag, or full commit SHA
make update-operand-manifests MCPLO_REF=v0.2.0
```

The Go updater in `tools/source-update` resolves a branch, tag, or full commit SHA once, then uses Go module metadata to derive the ODH fork's real Go version for that exact source commit. It keeps the canonical MCPLO requirement at `v0.0.0` and pins the ODH fork replacement to that real version. The module origin SHA must match the selected full source commit. The update synchronizes the fork replacement, `go.sum`, copied configuration, embedded installer, and `operand-image.json`. During `update`, it resolves the public Quay tag named for that full source commit, validates the OCI index digest and every runnable platform image's source/repository labels, then records the selected index reference and source SHA. Each runnable platform must supply at least one of `git.commit`, `org.opencontainers.image.revision`, or `vcs-ref`; every supplied recognized source label must equal the full source commit (empty or conflicting values are rejected). The `git.url` repository label remains required. A missing published build or mismatched source fails before outputs are installed. Module files, configuration, installer, image record and workflow outputs are restored on update failure.

The tag is mutable, so `make verify-operand-manifests` uses the committed source/image record and does not look up the tag again. It regenerates from that exact source and image and byte-compares the config and installer output. It still fetches pinned source and Go tooling; it is not an offline or disconnected-cluster test. Update-time checks validate published labels and registry content hashes, not cryptographic signatures or provenance attestations. On release branches, use the matching MCPLO release branch rather than `main`.

A GitHub Actions workflow (`.github/workflows/update-operand-manifests.yml`) runs daily at 06:00 UTC on `main` and opens a PR for source/image record or generated-file changes. Release updates currently require manual dispatch on the release branch.

## Testing

### Verification

`make verify` checks local generated code, formatting, module dependencies, and updater unit tests without fetching and regenerating MCPLO source. Cold-cache tool installation and module resolution can still require network access.

Run `make verify-operand-manifests` explicitly to check copied operand configuration and the embedded installer against the saved source/image pins. This fetches the pinned source and builds checkout-local generator tools; it does not advance the pins. CI runs both verification targets.

### Unit tests

```bash
make unit-test
```

`make test` runs unit tests with code generation, formatting, vet, and coverage. Both unit-test targets include the source updater and exclude live-cluster E2E tests:

```bash
make test
```

### E2E tests

Set `KUBECONFIG` to your test cluster. Deploy the module operator and prepare a Managed `MCPLifecycleOperator/default` before running tests. The Kind workflow above includes the sample CR. On RHOAI, enable the MCP component through DSC before running the suite.

The suite discovers the installation namespace from the module operator Deployment labels. Set `SYSTEM_NAMESPACE` to select a namespace explicitly when needed, such as when multiple installations exist. Discovery fails if no installation is found, multiple installations match, or the API request fails.

```bash
# Run all checks compatible with the prepared installation.
make e2e-test
```

The suite automatically skips standalone tests when the singleton has an owner reference, as it does on RHOAI and ODH. An unowned singleton runs the full suite. No label filter is required.

Labels:

- `lifecycle`: changes the singleton's management state, disrupts the operand Pod, or deletes the singleton. Requires a disposable installation and exclusive access to the component. Each test restores readiness during cleanup.
- `standalone`: deletes the singleton and checks operand garbage collection. Automatically skipped when the singleton has a parent owner.

Unlabeled checks leave the singleton unchanged. Use one Ginkgo process. The suite leaves the installation running after tests.

Install optional Gateway test prerequisites with `make deploy-gateway-crds`. Pass Ginkgo options through `E2E_TEST_ARGS`, such as `-ginkgo.v` or `-ginkgo.junit-report=/tmp/mcplmo-junit.xml`.

`make e2e-test` sets the Go test timeout to 15 minutes by default. Override it with `E2E_TEST_TIMEOUT`, for example `make e2e-test E2E_TEST_TIMEOUT=30m`. The container image uses the shared runner, which invokes a precompiled test binary directly without adding a suite timeout; that binary's timeout defaults to disabled unless `-test.timeout` is passed as an image argument. Individual polling waits have a fixed 5-minute timeout.

### Containerized E2E suite

See [MCPLMO E2E image](test/e2e/README.md) for external-cluster invocation, release tags, permissions, and the JUnit/log/artifact contract.

## CI

GitHub Actions workflows run on this repository:

- **E2E Tests** (`.github/workflows/e2e.yml`) - runs on PRs and pushes to `main`. Stands up a Kind cluster, deploys the operator, and runs E2E tests.
- **Verify** (`.github/workflows/verify.yml`) - runs on PRs. Runs `make verify` and explicitly runs `make verify-operand-manifests` for pinned-source regeneration and drift checking.
- **Update Operand Manifests** (`.github/workflows/update-operand-manifests.yml`) - daily cron and manual dispatch. Updates the source/image record and generated operand files together.

Additionally, Tekton PipelineRuns in `.tekton/` handle Konflux/Red Hat builds for pull requests and pushes.

## AI Agents

See [AGENT.md](AGENT.md) for rules and conventions that AI tools must follow when working on this project.
