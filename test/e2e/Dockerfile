FROM registry.access.redhat.com/ubi9/go-toolset@sha256:c2bd0309bf78ae317024fdfc5602a31704e0dba9572f6d4b0bd4b681033dc965 AS builder
ARG TARGETOS=linux
ARG TARGETARCH

ENV GOTOOLCHAIN=local
USER root
WORKDIR /workspace
COPY go.mod go.sum ./
RUN go mod download
COPY api/ api/
COPY internal/ internal/
COPY test/ test/

ENV GOEXPERIMENT=strictfipsruntime
RUN CGO_ENABLED=1 GOEXPERIMENT=strictfipsruntime \
    GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go test -c -race -tags=e2e -o /e2e-tests ./test/e2e/

RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -o /gotestsum gotest.tools/gotestsum
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -o /test2json "$(go env GOROOT)/src/cmd/test2json"
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -ldflags="-X 'github.com/kubernetes-sigs/mcp-lifecycle-operator/test/e2e/runner.TestPackages=e2e=/e2e/e2e-tests'" \
    -o /e2e-run ./test/e2e/cmd/run/

FROM --platform=$TARGETPLATFORM registry.access.redhat.com/ubi9/ubi-minimal@sha256:4b79c93106c14ad1c234907f83441ff0e4f8d24e78426a36e159098e9f7e6fa5
ARG USER_ID=65532
WORKDIR /e2e
COPY --from=builder /e2e-tests ./e2e-tests
COPY --from=builder /gotestsum /usr/local/bin/gotestsum
COPY --from=builder /test2json /usr/local/bin/test2json
COPY --from=builder /e2e-run ./e2e-run
RUN mkdir -p /artifacts && chown ${USER_ID}:${USER_ID} /artifacts

USER ${USER_ID}:${USER_ID}

LABEL com.redhat.component=mcp-lifecycle-module-operator
LABEL description="MCP lifecycle module operator e2e tests"
LABEL io.k8s.description="MCP lifecycle module operator e2e tests"
LABEL io.k8s.display-name="MCP lifecycle module operator e2e tests"
LABEL io.openshift.tags="openshift,mcp,operator,e2e"
LABEL name="mcp-lifecycle-operator-beta/mcp-lifecycle-module-operator-e2e-rhel9"
LABEL summary="MCP lifecycle module operator e2e tests"

ENTRYPOINT ["/e2e/e2e-run"]
CMD []
