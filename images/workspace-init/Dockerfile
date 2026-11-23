# code-server is fetched as a versioned ARM64 standalone release. Keep the
# download tools out of the final Workspace image.
ARG HAKO_IMAGE_ARCH=arm64
ARG HAKO_WORKSPACE_BASE_IMAGE=public.ecr.aws/lambda/microvms@sha256:b6a34591182b4a1969f57eb24c812582d0c52dc2e40b75b73c0df371bb7aef82
FROM --platform=linux/${HAKO_IMAGE_ARCH} ${HAKO_WORKSPACE_BASE_IMAGE} AS code-server
ARG HAKO_IMAGE_ARCH
ARG CODE_SERVER_VERSION
ARG CODE_SERVER_SHA256
RUN test -n "${CODE_SERVER_VERSION}" \
    && test -n "${CODE_SERVER_SHA256}" \
    && case "${HAKO_IMAGE_ARCH}" in amd64|arm64) ;; *) exit 1 ;; esac \
    && dnf install -y --setopt=install_weak_deps=0 curl-minimal tar gzip \
    && curl --fail --location --silent --show-error --retry 3 \
      "https://github.com/coder/code-server/releases/download/v${CODE_SERVER_VERSION}/code-server-${CODE_SERVER_VERSION}-linux-${HAKO_IMAGE_ARCH}.tar.gz" \
      --output /tmp/code-server.tar.gz \
    && echo "${CODE_SERVER_SHA256}  /tmp/code-server.tar.gz" | sha256sum --check \
    && mkdir -p /opt/hako-code-server \
    && tar -xzf /tmp/code-server.tar.gz --strip-components=1 -C /opt/hako-code-server \
    && test -x /opt/hako-code-server/bin/code-server \
    && rm -f /tmp/code-server.tar.gz \
    && dnf clean all

FROM --platform=linux/${HAKO_IMAGE_ARCH} ${HAKO_WORKSPACE_BASE_IMAGE} AS nerdctl
ARG HAKO_IMAGE_ARCH
ARG NERDCTL_FULL_VERSION
ARG NERDCTL_FULL_SHA256
ARG CNI_PLUGINS_VERSION
RUN dnf install -y tar gzip && dnf clean all
COPY nerdctl-full-linux-${HAKO_IMAGE_ARCH}.tar.gz /tmp/nerdctl-full-linux-${HAKO_IMAGE_ARCH}.tar.gz
RUN case "${NERDCTL_FULL_VERSION}" in ''|*[!0-9.]* ) exit 1 ;; esac \
    && echo "${CNI_PLUGINS_VERSION}" | grep -Eq '^v?[0-9]+\.[0-9]+\.[0-9]+$' \
    && cni_version="${CNI_PLUGINS_VERSION#v}" \
    && test "$(printf '1.1.0\n%s\n' "${cni_version}" | sort -V | head -n 1)" = "1.1.0" \
    && echo "${NERDCTL_FULL_SHA256}" | grep -Eq '^[[:xdigit:]]{64}$' \
    && echo "${NERDCTL_FULL_SHA256}  /tmp/nerdctl-full-linux-${HAKO_IMAGE_ARCH}.tar.gz" | sha256sum --check \
    && tar -tzf /tmp/nerdctl-full-linux-${HAKO_IMAGE_ARCH}.tar.gz | grep -Fxq libexec/cni/firewall \
    && tar -xOf /tmp/nerdctl-full-linux-${HAKO_IMAGE_ARCH}.tar.gz share/doc/nerdctl-full/README.md | grep -Fx -- "- CNI plugins: ${CNI_PLUGINS_VERSION}" \
    && mkdir -p /opt/hako-nerdctl \
    && tar -xzf /tmp/nerdctl-full-linux-${HAKO_IMAGE_ARCH}.tar.gz --no-same-owner --no-same-permissions -C /opt/hako-nerdctl \
    && test -x /opt/hako-nerdctl/bin/nerdctl \
    && test -x /opt/hako-nerdctl/bin/containerd \
    && test -x /opt/hako-nerdctl/bin/containerd-rootless.sh \
    && test -x /opt/hako-nerdctl/bin/rootlesskit \
    && /opt/hako-nerdctl/bin/nerdctl --version | grep -Fx "nerdctl version ${NERDCTL_FULL_VERSION}" \
    && rm -f /tmp/nerdctl-full-linux-${HAKO_IMAGE_ARCH}.tar.gz

FROM --platform=linux/${HAKO_IMAGE_ARCH} ${HAKO_WORKSPACE_BASE_IMAGE}
ARG HAKO_IMAGE_ARCH
ARG HAKO_WORKSPACE_SERVICES_ENABLED=false
ARG HAKO_WORKSPACE_AGENT_VSOCK_ENABLED=false
ARG SLIRP4NETNS_VERSION
ARG SLIRP4NETNS_SHA256
COPY slirp4netns-${HAKO_IMAGE_ARCH} /tmp/slirp4netns-${HAKO_IMAGE_ARCH}

# Pin the helper RPM version at image build time. The build fails closed if a
# version is omitted, so the produced image can be reproduced and audited.
ARG EFS_UTILS_RPM_VERSION
ARG OPENSSH_RPM_VERSION
RUN test -n "${EFS_UTILS_RPM_VERSION}" \
    && test -n "${OPENSSH_RPM_VERSION}" \
    && case "${HAKO_WORKSPACE_SERVICES_ENABLED}" in true|false) ;; *) exit 1 ;; esac \
    && case "${HAKO_WORKSPACE_AGENT_VSOCK_ENABLED}" in true|false) ;; *) exit 1 ;; esac \
    && case "${SLIRP4NETNS_VERSION}" in ''|*[!0-9.]* ) exit 1 ;; esac \
    && echo "${SLIRP4NETNS_SHA256}" | grep -Eq '^[[:xdigit:]]{64}$' \
    && echo "${SLIRP4NETNS_SHA256}  /tmp/slirp4netns-${HAKO_IMAGE_ARCH}" | sha256sum --check \
    && dnf install -y --setopt=install_weak_deps=0 "amazon-efs-utils-${EFS_UTILS_RPM_VERSION}" "openssh-server-${OPENSSH_RPM_VERSION}" "openssh-clients-${OPENSSH_RPM_VERSION}" shadow-utils \
    && install -m 0755 /tmp/slirp4netns-${HAKO_IMAGE_ARCH} /usr/local/bin/slirp4netns \
    && chmod u+s /usr/bin/newuidmap /usr/bin/newgidmap \
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
COPY hako-local-workspace-guest-init /usr/local/bin/hako-local-workspace-guest-init
COPY trusted-keys.json /etc/hako/workspace-agent-public-keys.json
COPY --from=code-server /opt/hako-code-server/ /opt/hako-code-server/
COPY --from=nerdctl /opt/hako-nerdctl/ /usr/local/

RUN ln -s /opt/hako-code-server/bin/code-server /usr/local/bin/code-server \
    && chmod 0755 /usr/local/bin/hako-workspace-init /usr/local/bin/hako-workspace-agent /usr/local/bin/hako-local-workspace-guest-init \
    && chmod 0444 /etc/hako/workspace-agent-public-keys.json \
    && test -x /usr/local/bin/nerdctl \
    && test -x /usr/local/bin/containerd-rootless.sh \
    && test -x /usr/bin/newuidmap \
    && test -x /usr/bin/newgidmap \
    && test -u /usr/bin/newuidmap \
    && test -u /usr/bin/newgidmap \
    && test -x /usr/local/bin/slirp4netns \
    && rm -f /tmp/slirp4netns-${HAKO_IMAGE_ARCH}

EXPOSE 8080
ENV HAKO_WORKSPACE_AGENT_TRUSTED_KEYS_FILE=/etc/hako/workspace-agent-public-keys.json
ENV HAKO_WORKSPACE_SERVICES_ENABLED=${HAKO_WORKSPACE_SERVICES_ENABLED}
ENV HAKO_WORKSPACE_AGENT_VSOCK_ENABLED=${HAKO_WORKSPACE_AGENT_VSOCK_ENABLED}
ENV HAKO_WORKSPACE_SSH_ENABLED=true
ENV HAKO_SLIRP4NETNS_VERSION=${SLIRP4NETNS_VERSION}
USER 0:0
ENTRYPOINT ["/usr/local/bin/hako-workspace-init"]
