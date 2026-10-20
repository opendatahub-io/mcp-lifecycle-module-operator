# AGENT.md

## Always Keep in Mind

Act like a professional software developer and engineer. Adhere to architecture, naming conventions and coding standards in this codebase. If unsure, read similar files and get inspiration from the rest of the codebase. If introducing new features, make sure to cover them via unit tests and don't forget to take edge cases into account.

## Project Overview

Kubernetes operator (controller-runtime) for managing the lifecycle of the [MCP Lifecycle Operator](https://github.com/opendatahub-io/mcp-lifecycle-operator) as a module within [Open Data Hub](https://opendatahub.io/) (ODH). Written in Go, uses Ginkgo/Gomega for tests.

This is a **module operator** - it does not implement business logic. Its sole job is to deploy, reconcile, and garbage-collect the operand based on a `MCPLifecycleOperator` CR and a platform-managed ConfigMap. It follows **v2 of the [Onboarding Guide for ODH Operator Modules](https://docs.google.com/document/d/1FgN_U-6XH8M-Mu6XNeldUlTPsnw7UyPCWg5NVJJdYnw)**.

## Testing Strategy

Before committing, test locally following the table below:

| If changed | Target | Description |
|---|---|---|
| `*.go` files | `make unit-test` | Unit tests (fast, no codegen) |
| `*.go` files | `make test` | Unit tests with codegen + fmt + vet |
| `api/` types or RBAC markers | `make manifests generate` | Regenerate CRDs, DeepCopy, and RBAC |
| Significant changes or E2E changes | `make e2e-test` | Prepared dedicated cluster; see [CONTRIBUTING.md](CONTRIBUTING.md) for prerequisites and test selection. |

## Key Architecture Rules

- The `MCPLifecycleOperator` CR is **cluster-scoped** and its name must be `default` (enforced by CEL validation).
- The CRD is at API version `v1alpha1`. Do not promote without explicit instruction.
- Platform config (operand image, operand namespace) comes from environment variables (`RELATED_IMAGE_*`, `SYSTEM_NAMESPACE`), **not** the CR spec. Do not add platform-managed fields to `MCPLifecycleOperatorSpec`.
- Operand manifests are **vendored** at `internal/controller/resources/mcp-lifecycle-operator.yaml`. Do not edit by hand - use `make update-operand-manifests`.
- All managed resources must carry the label `platform.opendatahub.io/part-of: mcplifecycleoperator`.

## Boundaries

### Always Do

- Format Go files with `gofmt` and organize imports with `goimports`
- Run `make manifests generate` after modifying `api/` types or kubebuilder RBAC markers
- Run `make test` before considering any change complete
- Read `CONTRIBUTING.md` for development setup and workflow

### Ask First

- Security-related code changes (authentication, credentials, secrets handling)
- API changes to the `MCPLifecycleOperator` CRD
- Adding new dependencies
- Modifying CI/GitHub Actions workflows
- Promoting the CRD version beyond `v1alpha1`

### Never Do

- Commit secrets, API keys, or credentials
- Delete files without explicit user approval
- Force push to main/master branch
- Skip tests
- Hand-edit vendored operand manifests (`internal/controller/resources/`)
- Add platform-managed fields to the CR spec

## Important Documentation

Read these files to understand the project setup, conventions, and development workflow:

- `README.md` - project overview, architecture, installation, CR reference
- `CONTRIBUTING.md` - development setup, make targets, testing, CI workflows

After implementing a feature or making significant changes, check whether these docs need updating. The CR reference table in README.md must stay in sync with `api/v1alpha1/mcplifecycleoperator_types.go`.