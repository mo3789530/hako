FROM golang:1.27.2 AS go-toolchain

FROM public.ecr.aws/lambda/microvms:al2023-minimal

# Repository execution is fixed to `go test ./...`; the MicroVM needs the Go
# toolchain and Git, but receives no AWS credential or controller signing key.
RUN dnf install -y git ca-certificates shadow-utils && dnf clean all
COPY --from=go-toolchain /usr/local/go /usr/local/go
COPY hako-agent /usr/local/bin/hako-agent
COPY trusted-keys.json /etc/hako/agent-trusted-keys.json

ENV PATH="/usr/local/go/bin:${PATH}" \
    HAKO_AGENT_TRUSTED_KEYS_FILE=/etc/hako/agent-trusted-keys.json \
    HAKO_AGENT_SOURCE_DIRECTORY=/workspace/source \
    HAKO_GO_MODULE_CACHE=/go/pkg/mod \
    HOME=/tmp

RUN mkdir -p /workspace/source /go/pkg/mod /tmp/hako-go-cache \
    && chown -R 65532:65532 /workspace /go /tmp/hako-go-cache \
    && chmod 0755 /workspace /workspace/source /go /go/pkg /go/pkg/mod /tmp/hako-go-cache

WORKDIR /workspace/source
EXPOSE 8080
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/hako-agent"]
