FROM public.ecr.aws/amazonlinux/amazonlinux:2023

ARG ACTIONS_RUNNER_VERSION
ARG ACTIONS_RUNNER_SHA256

RUN test -n "${ACTIONS_RUNNER_VERSION}" && test -n "${ACTIONS_RUNNER_SHA256}" && \
    dnf install -y ca-certificates git gzip tar shadow-utils && dnf clean all

RUN mkdir -p /etc/hako

LABEL org.opencontainers.image.version="${ACTIONS_RUNNER_VERSION}"

COPY actions-runner-linux-arm64.tar.gz /tmp/actions-runner-linux-arm64.tar.gz
COPY hako-actions-runner-guest /usr/local/bin/hako-actions-runner-guest
COPY actions-runner-bootstrap.pub /etc/hako/actions-runner-bootstrap.pub

RUN echo "${ACTIONS_RUNNER_SHA256}  /tmp/actions-runner-linux-arm64.tar.gz" | sha256sum -c - && \
    grep -q '^-----BEGIN PUBLIC KEY-----$' /etc/hako/actions-runner-bootstrap.pub && \
    ! grep -q 'PRIVATE KEY' /etc/hako/actions-runner-bootstrap.pub && \
    mkdir -p /opt/actions-runner /home/hako-runner && \
    tar -xzf /tmp/actions-runner-linux-arm64.tar.gz -C /opt/actions-runner && \
    rm -f /tmp/actions-runner-linux-arm64.tar.gz && \
    /opt/actions-runner/bin/installdependencies.sh && \
    groupadd --system hako-runner && \
    useradd --system --gid hako-runner --home-dir /home/hako-runner --shell /sbin/nologin hako-runner && \
    chown -R hako-runner:hako-runner /opt/actions-runner /home/hako-runner && \
    chmod 0644 /etc/hako/actions-runner-bootstrap.pub && \
    chmod 0755 /usr/local/bin/hako-actions-runner-guest

ENV HAKO_ACTIONS_RUNNER_ROOT=/opt/actions-runner
EXPOSE 8080
USER hako-runner
ENTRYPOINT ["/usr/local/bin/hako-actions-runner-guest"]
