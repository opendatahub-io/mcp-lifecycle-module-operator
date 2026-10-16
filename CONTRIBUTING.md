# Contributing

## Prerequisites

- Go 1.26+
- [Kind](https://kind.sigs.k8s.io/) (for local E2E testing)
- Docker or Podman
- `kubectl`

Build tools (`kustomize`, `controller-gen`) are downloaded automatically into `./bin/` by the Makefile.

## Make Targets

Run `make help` for the full list. Key targets:

| Target | Description |
|---|---|
| `make build` | Build the manager binary (runs codegen + fmt + vet first) |
| `make test` | Run unit tests with coverage (runs codegen first) |
| `make unit-test` | Run unit tests only (no codegen prerequisites) |
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

## Code Generation

This project uses [kubebuilder](https://book.kubebuilder.io/) markers for code generation:

- **CRD + RBAC manifests**: generated from `// +kubebuilder:rbac:...` and type annotations. Run `make manifests` after changing markers.
- **DeepCopy methods**: generated from types in `api/`. Run `make generate` after changing API types.

Always run `make manifests generate` before committing changes to API types or RBAC markers.

## Updating Operand Manifests

The operand (MCP Lifecycle Operator) manifests are vendored at `internal/controller/resources/mcp-lifecycle-operator.yaml`. They are obtained by running `kustomize build` on the midstream fork at [opendatahub-io/mcp-lifecycle-operator](https://github.com/opendatahub-io/mcp-lifecycle-operator).

To update them:

```bash
# From main branch (default)
make update-operand-manifests

# From a specific tag or branch
make update-operand-manifests MCPLO_REF=v0.2.0
```

A GitHub Actions workflow (`.github/workflows/update-operand-manifests.yml`) runs this weekly and opens a PR if manifests changed.

## Testing

### Unit tests

```bash
make unit-test
# or with codegen prerequisites:
make test
```

### E2E tests

Set `KUBECONFIG` to your test cluster. Deploy the module operator and prepare a Managed `MCPLifecycleOperator/default` before running tests. The Kind workflow above includes the sample CR. On RHOAI, enable the MCP component through DSC before running the suite.

The suite discovers the installation namespace from the module operator Deployment labels. Set `SYSTEM_NAMESPACE` to select a namespace explicitly when needed, such as when multiple installations exist. Discovery fails if no installation is found, multiple installations match, or the API request fails.

```bash
# Kind: run all checks.
make e2e-test

# RHOAI: run all compatible checks.
make e2e-test E2E_TEST_ARGS='-ginkgo.label-filter=!standalone'
```

Labels:

- `lifecycle`: changes the singleton's management state, disrupts the operand Pod, or deletes the singleton. Requires a disposable installation and exclusive access to the component. Each test restores readiness during cleanup.
- `standalone`: deletes the singleton and checks operand garbage collection. Requires an installation without a parent controller that recreates the singleton. Exclude this label on RHOAI.

Unlabeled checks leave the singleton unchanged. Use one Ginkgo process. The suite leaves the installation running after tests.

Install optional Gateway test prerequisites with `make deploy-gateway-crds`. Pass Ginkgo options through `E2E_TEST_ARGS`, such as `-ginkgo.v` or `-ginkgo.junit-report=/tmp/mcplmo-junit.xml`.

The suite timeout defaults to 15 minutes. Override it with `E2E_TEST_TIMEOUT`, for example `make e2e-test E2E_TEST_TIMEOUT=30m`. Individual polling waits have a fixed 5-minute timeout.

## CI

GitHub Actions workflows run on this repository:

- **E2E Tests** (`.github/workflows/e2e.yml`) - runs on PRs and pushes to `main`. Stands up a Kind cluster, deploys the operator, and runs E2E tests.
- **Verify** (`.github/workflows/verify.yml`) - runs on PRs. Checks formatting, linting, and codegen freshness.
- **Update Operand Manifests** (`.github/workflows/update-operand-manifests.yml`) - weekly cron and manual dispatch. Re-vendors operand manifests and opens a PR if they changed.

Additionally, Tekton PipelineRuns in `.tekton/` handle Konflux/Red Hat builds for pull requests and pushes.

## AI Agents

See [AGENT.md](AGENT.md) for rules and conventions that AI tools must follow when working on this project.
