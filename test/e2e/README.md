# MCPLMO E2E image

The image runs the available MCPLMO suite against an existing Kubernetes/RHOAI installation. It does not install the product or provision a cluster.

Image: `quay.io/opendatahub/odh-mcp-lifecycle-module-operator-e2e`.

| Build | Tags |
|---|---|
| Pull request to main | `odh-pr`, `odh-pr-<commit>` |
| Main | `main`, `odh-stable` |
| Release branch | Branch name, for example `rhoai-3.6` |

Release branches mirror downstream names, excluding `rhoai-3.5`, `rhoai-3.6-ea.1` and `rhoai-3.6-ea.2`. Konflux uses its central pipeline's default build platforms. Inspect the published image index rather than assuming additional architectures are present. Use an image digest when recording validation results.

## Cluster requirements

Run only on an authorized non-production cluster, with no concurrent copy of this suite. Current tests delete/recreate `MCPLifecycleOperator/default`, set it to `Removed`, temporarily disrupt the operand Pod, and create fixed-name resources. They are not read-only probes. Platform DSC reconciliation can cause tests to fail; changing that behavior is separate suite work.

Provide:

- A readable, self-contained kubeconfig pointing to the test cluster. Referenced certificate/key files and exec-auth plugins must also be available inside the container; host paths and credential helpers are not automatically inherited.
- `SYSTEM_NAMESPACE`, an optional explicit selector for the installed applications namespace containing the module operator and operand. If unset, the suite discovers the namespace from module-operator Deployment labels; this requires permission to list Deployments cluster-wide. `mcp-lifecycle-module-operator-system` is the standalone Kind example namespace, not a suite default.
- Credentials allowed to read cluster CRDs/webhook configuration, manage the singleton MCPLifecycleOperator, inspect and mutate operand Pods/Deployments, and manage the suite's namespaced resources.
- A writable artifact mount. The image runs nonroot.

The suite does not assume in-cluster ServiceAccount authentication in place of a kubeconfig. No runtime Go compiler or kubectl is required.

## Run

Example for rootless Podman, using your UID for writable host artifacts:

```bash
export KUBECONFIG=/absolute/path/to/test-kubeconfig
export SYSTEM_NAMESPACE=redhat-ods-applications
export IMAGE=quay.io/opendatahub/odh-mcp-lifecycle-module-operator-e2e:rhoai-3.6
mkdir -p artifacts

podman run --rm --userns=keep-id --user "$(id -u):$(id -g)" \
  -v "$KUBECONFIG:/kubeconfig:ro" \
  -v "$PWD/artifacts:/artifacts" \
  -e KUBECONFIG=/kubeconfig \
  -e SYSTEM_NAMESPACE \
  "$IMAGE" >artifacts/container.log 2>&1
status=$?
printf 'Container exit status: %s\n' "$status"
```

On SELinux hosts, bind mounts may need appropriate labels (for example `:Z` on a private artifact directory). Do not disable SELinux or make credentials world-readable. For Kubernetes/ITS jobs, supply readable kubeconfig and writable artifact volumes through the job's security context.

The entrypoint is `/e2e/e2e-run`. It runs the whole compiled `./test/e2e` package by default. The shared runner forwards arguments without adding a suite timeout. Since it invokes a precompiled Go test binary directly, the test-binary timeout defaults to disabled. Pass an explicit timeout to set one:

```bash
# Append after the image name in the command above:
-test.timeout=30m
```

## Results contract

| Output | Default location |
|---|---|
| JUnit XML | `/artifacts/e2e-results/junit.xml` |
| Go test event JSONL | `/artifacts/e2e-results/log.jsonl` |
| Human-readable output | Container stdout/stderr |

The example also saves stdout/stderr as `artifacts/container.log`. Jenkins/ITS must collect the mounted artifact directory and container logs even when the process exits nonzero. The suite does not currently produce a separate cluster dump or diagnostic archive.

The wrapper propagates the shared runner's exit code. Nonzero can mean a failed test, timeout, invalid configuration, or inability to execute/write results. Preserve the status and inspect the logs and XML to distinguish these cases. Startup failures can occur before reports exist.

Reporting uses the shared MCPLO runner's default `rhoai` XML rewrite for Jenkins/ReportPortal. The component remains `mcp-lifecycle-operator`, displayed as `Mcp Lifecycle Operator`; MCPLMO classnames use `Module`, with package/test name `e2e/TestE2E`. Do not register a separate MCPLMO top-level result or rewrite the XML again.

JUnit contains the Go-level `TestE2E` result, not a testcase for every Ginkgo spec. Individual spec skips, including optional gateway tests when CRDs are absent, are visible in the test output/JSONL. A root XML error counter is not necessarily a testcase `<error>`; retain the exact shared report. A missing-Go metadata warning in a binary-only runtime does not itself indicate test failure.

Supported shared-runner overrides include:

| Variable | Default / purpose |
|---|---|
| `ARTIFACTS` | `/artifacts`, artifact root |
| `E2E_RESULTS_DIR` | `e2e-results`, results subdirectory |
| `E2E_GO_TEST_VERBOSITY` | `testname`, gotestsum output format |
| `E2E_JUNIT_REPORT_MODE` | `rhoai`; keep this for current Jenkins consumers |
| `E2E_REPO_ABBR` | `module`; keep repository results distinct |
| `E2E_COMPONENT` | `mcp-lifecycle-operator`; retain the shared component identity |

Keep `E2E_COUNT=1`; Ginkgo does not support Go test repetition through this setting. The inherited `E2E_GOTESTSUM_BIN` and `E2E_TEST2JSON_BIN` overrides are for alternate helper paths, not cluster configuration. Verbatim reporting or repository renaming requires separate consumer coordination.

## Validation and release alignment

`go.mod` keeps the shared runner's canonical MCPLO requirement at `v0.0.0` and pins its ODH fork replacement to the real Go module version resolved for the selected source commit, with checksums in `go.sum`. `make update-operand-manifests` resolves one immutable source commit and synchronizes the fork replacement, checksums, source/image record, vendored manifests, and embedded installer. The fork version's full Go module origin SHA and `operand-image.json` commit must match; verification checks that relationship without re-resolving the mutable image tag. The embedded default operand image remains separate from the runtime `RELATED_IMAGE_ODH_MCP_LIFECYCLE_OPERATOR_IMAGE` override.

For independent validation, record the image digest/platforms, MCPLMO commit, MCPLO source pin, RHOAI/OpenShift versions, namespace, invocation, exit status, and collected reports/logs. Passing, skipped, and failing suite results are acceptable for image delivery when execution and reporting correctly represent them. Fix image/runtime/reporting wiring failures here; suite conformance is separate work. Complete this validation before Jenkins registration under RHOAIENG-100285.
