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
    && chmod 0755 /workspace

COPY hako-workspace-init /usr/local/bin/hako-workspace-init

EXPOSE 8080
USER 0:0
ENTRYPOINT ["/usr/local/bin/hako-workspace-init"]
