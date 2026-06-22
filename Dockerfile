# -----------------------------------------------------------------------------
# Build arguments
# -----------------------------------------------------------------------------
ARG BUILDER_IMAGE=registry.access.redhat.com/ubi9/go-toolset:latest
ARG BASE_IMAGE=registry.access.redhat.com/ubi9/ubi-minimal:latest

# -----------------------------------------------------------------------------
# Builder stage
# -----------------------------------------------------------------------------
FROM ${BUILDER_IMAGE} AS builder

ARG TARGETOS
ARG TARGETARCH
ARG TARGETPLATFORM

WORKDIR /workspace

# Copy source code
COPY . .

USER root

# Enable strict FIPS runtime support during build
ENV GOEXPERIMENT=strictfipsruntime
# Build the manager binary
RUN make build GO_BUILD_ENV="GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH}"

# -----------------------------------------------------------------------------
# Runtime stage
# -----------------------------------------------------------------------------
FROM --platform=$TARGETPLATFORM ${BASE_IMAGE}

WORKDIR /

# Copy the controller binary
COPY --from=builder /workspace/bin/manager .

# Copy license files
RUN mkdir /licenses
COPY --from=builder /workspace/LICENSE /licenses/

# Run as non-root
USER 65532:65532

# -----------------------------------------------------------------------------
# Labels for enterprise contract
# -----------------------------------------------------------------------------
LABEL com.redhat.component=mcp-lifecycle-module-operator
LABEL cpe="cpe:/a:redhat:mcp_lifecycle_operator:0.1::el9"
LABEL description="MCP lifecycle module operator"
LABEL io.k8s.description="MCP lifecycle module operator"
LABEL io.k8s.display-name="MCP lifecycle module operator"
LABEL io.openshift.tags="openshift,mcp,operator"
LABEL name="mcp-lifecycle-operator-beta/mcp-lifecycle-module-rhel9-operator"
LABEL release=0.1.0
LABEL url="https://github.com/opendatahub-io/mcp-lifecycle-module-operator"
LABEL vendor="Red Hat, Inc."
LABEL version=0.1.0
LABEL summary="MCP lifecycle module operator"
LABEL konflux.additional-tags="latest"

# -----------------------------------------------------------------------------
# Entrypoint
# -----------------------------------------------------------------------------
ENTRYPOINT ["/manager"]
