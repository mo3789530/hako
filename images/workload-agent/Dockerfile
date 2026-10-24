FROM --platform=linux/arm64 docker.io/library/golang:1.27.2@sha256:a83e961ad968e2e7abc2a59554e356f00c2a270a15fbb39f2c84a4caf13be9c3 AS go-toolchain

FROM --platform=linux/arm64 public.ecr.aws/lambda/microvms:al2023-minimal@sha256:b6a34591182b4a1969f57eb24c812582d0c52dc2e40b75b73c0df371bb7aef82

# Repository execution is fixed to `go test ./...`; the MicroVM needs the Go
# toolchain and Git, but receives no AWS credential or controller signing key.
# hako-agent remains root so it can own the isolated runtime socket. Every
# repository-controlled command is explicitly launched as 65532:65532.
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
USER 0:0
ENTRYPOINT ["/usr/local/bin/hako-agent"]
