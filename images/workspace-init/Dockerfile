# code-server is fetched as a versioned ARM64 standalone release. Keep the
# download tools out of the final Workspace image.
FROM --platform=linux/arm64 public.ecr.aws/lambda/microvms:al2023-minimal@sha256:b6a34591182b4a1969f57eb24c812582d0c52dc2e40b75b73c0df371bb7aef82 AS code-server
ARG CODE_SERVER_VERSION
ARG CODE_SERVER_SHA256
RUN test -n "${CODE_SERVER_VERSION}" \
    && test -n "${CODE_SERVER_SHA256}" \
    && dnf install -y --setopt=install_weak_deps=False curl-minimal tar gzip \
    && curl --fail --location --silent --show-error --retry 3 \
      "https://github.com/coder/code-server/releases/download/v${CODE_SERVER_VERSION}/code-server-${CODE_SERVER_VERSION}-linux-arm64.tar.gz" \
      --output /tmp/code-server.tar.gz \
    && echo "${CODE_SERVER_SHA256}  /tmp/code-server.tar.gz" | sha256sum --check \
    && mkdir -p /opt/hako-code-server \
    && tar -xzf /tmp/code-server.tar.gz --strip-components=1 -C /opt/hako-code-server \
    && test -x /opt/hako-code-server/bin/code-server \
    && rm -f /tmp/code-server.tar.gz \
    && dnf clean all

FROM --platform=linux/arm64 public.ecr.aws/lambda/microvms:al2023-minimal@sha256:b6a34591182b4a1969f57eb24c812582d0c52dc2e40b75b73c0df371bb7aef82 AS nerdctl
ARG NERDCTL_FULL_VERSION
ARG NERDCTL_FULL_SHA256
RUN dnf install -y tar gzip && dnf clean all
COPY nerdctl-full-linux-arm64.tar.gz /tmp/nerdctl-full-linux-arm64.tar.gz
RUN case "${NERDCTL_FULL_VERSION}" in ''|*[!0-9.]* ) exit 1 ;; esac \
    && echo "${NERDCTL_FULL_SHA256}" | grep -Eq '^[[:xdigit:]]{64}$' \
    && echo "${NERDCTL_FULL_SHA256}  /tmp/nerdctl-full-linux-arm64.tar.gz" | sha256sum --check \
    && mkdir -p /opt/hako-nerdctl \
    && tar -xzf /tmp/nerdctl-full-linux-arm64.tar.gz --no-same-owner --no-same-permissions -C /opt/hako-nerdctl \
    && test -x /opt/hako-nerdctl/bin/nerdctl \
    && test -x /opt/hako-nerdctl/bin/containerd \
    && test -x /opt/hako-nerdctl/bin/containerd-rootless.sh \
    && test -x /opt/hako-nerdctl/bin/rootlesskit \
    && /opt/hako-nerdctl/bin/nerdctl --version | grep -Fx "nerdctl version ${NERDCTL_FULL_VERSION}" \
    && rm -f /tmp/nerdctl-full-linux-arm64.tar.gz

FROM --platform=linux/arm64 public.ecr.aws/lambda/microvms:al2023-minimal@sha256:b6a34591182b4a1969f57eb24c812582d0c52dc2e40b75b73c0df371bb7aef82
ARG HAKO_WORKSPACE_SERVICES_ENABLED=false
ARG SLIRP4NETNS_VERSION
ARG SLIRP4NETNS_SHA256
COPY slirp4netns-aarch64 /tmp/slirp4netns-aarch64

# Pin the helper RPM version at image build time. The build fails closed if a
# version is omitted, so the produced image can be reproduced and audited.
ARG EFS_UTILS_RPM_VERSION
ARG OPENSSH_RPM_VERSION
RUN test -n "${EFS_UTILS_RPM_VERSION}" \
    && test -n "${OPENSSH_RPM_VERSION}" \
    && case "${HAKO_WORKSPACE_SERVICES_ENABLED}" in true|false) ;; *) exit 1 ;; esac \
    && case "${SLIRP4NETNS_VERSION}" in ''|*[!0-9.]* ) exit 1 ;; esac \
    && echo "${SLIRP4NETNS_SHA256}" | grep -Eq '^[[:xdigit:]]{64}$' \
    && echo "${SLIRP4NETNS_SHA256}  /tmp/slirp4netns-aarch64" | sha256sum --check \
    && dnf install -y --setopt=install_weak_deps=False "amazon-efs-utils-${EFS_UTILS_RPM_VERSION}" "openssh-server-${OPENSSH_RPM_VERSION}" "openssh-clients-${OPENSSH_RPM_VERSION}" shadow-utils \
    && install -m 0755 /tmp/slirp4netns-aarch64 /usr/local/bin/slirp4netns \
    && dnf clean all \
    && test -x /sbin/mount.efs \
    && test -x /usr/sbin/sshd \
    && test -x /usr/bin/ssh-keygen \
    && mkdir -p /run/hako /workspace \
    && chmod 0700 /run/hako \
    && chmod 0755 /workspace \
    && mkdir -p /etc/hako

COPY hako-workspace-init /usr/local/bin/hako-workspace-init
COPY hako-workspace-agent /usr/local/bin/hako-workspace-agent
COPY trusted-keys.json /etc/hako/workspace-agent-public-keys.json
COPY --from=code-server /opt/hako-code-server/ /opt/hako-code-server/
COPY --from=nerdctl /opt/hako-nerdctl/ /usr/local/

RUN ln -s /opt/hako-code-server/bin/code-server /usr/local/bin/code-server \
    && chmod 0755 /usr/local/bin/hako-workspace-init /usr/local/bin/hako-workspace-agent \
    && chmod 0444 /etc/hako/workspace-agent-public-keys.json \
    && test -x /usr/local/bin/nerdctl \
    && test -x /usr/local/bin/containerd-rootless.sh \
    && test -x /usr/bin/newuidmap \
    && test -x /usr/bin/newgidmap \
    && test -u /usr/bin/newuidmap \
    && test -u /usr/bin/newgidmap \
    && test -x /usr/local/bin/slirp4netns \
    && rm -f /tmp/slirp4netns-aarch64

EXPOSE 8080
ENV HAKO_WORKSPACE_AGENT_TRUSTED_KEYS_FILE=/etc/hako/workspace-agent-public-keys.json
ENV HAKO_WORKSPACE_SERVICES_ENABLED=${HAKO_WORKSPACE_SERVICES_ENABLED}
ENV HAKO_WORKSPACE_SSH_ENABLED=true
ENV HAKO_SLIRP4NETNS_VERSION=${SLIRP4NETNS_VERSION}
USER 0:0
ENTRYPOINT ["/usr/local/bin/hako-workspace-init"]
