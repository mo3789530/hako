FROM public.ecr.aws/lambda/microvms:al2023-minimal

# Pin the helper RPM version at image build time. The build fails closed if a
# version is omitted, so the produced image can be reproduced and audited.
ARG EFS_UTILS_RPM_VERSION
RUN test -n "${EFS_UTILS_RPM_VERSION}" \
    && dnf install -y --setopt=install_weak_deps=False "amazon-efs-utils-${EFS_UTILS_RPM_VERSION}" \
    && dnf clean all \
    && test -x /sbin/mount.efs \
    && mkdir -p /run/hako /workspace \
    && chmod 0700 /run/hako \
    && chmod 0755 /workspace \
    && mkdir -p /etc/hako

COPY hako-workspace-init /usr/local/bin/hako-workspace-init
COPY hako-workspace-agent /usr/local/bin/hako-workspace-agent
COPY trusted-keys.json /etc/hako/workspace-agent-public-keys.json

RUN chmod 0755 /usr/local/bin/hako-workspace-init /usr/local/bin/hako-workspace-agent \
    && chmod 0444 /etc/hako/workspace-agent-public-keys.json

EXPOSE 8080
ENV HAKO_WORKSPACE_AGENT_TRUSTED_KEYS_FILE=/etc/hako/workspace-agent-public-keys.json
USER 0:0
ENTRYPOINT ["/usr/local/bin/hako-workspace-init"]
