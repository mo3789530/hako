ARG ACTIONS_RUNNER_VERSION=2.338.1
ARG ACTIONS_RUNNER_BASE_IMAGE=ghcr.io/actions/actions-runner@sha256:ee680dc9fe83da474e3c51d642f8bfac0d920da26735e3d5e4171ab8f2f45a87

FROM ${ACTIONS_RUNNER_BASE_IMAGE}

ARG ACTIONS_RUNNER_VERSION
ARG ACTIONS_RUNNER_BASE_IMAGE

LABEL org.opencontainers.image.version="${ACTIONS_RUNNER_VERSION}" \
      org.opencontainers.image.base.name="${ACTIONS_RUNNER_BASE_IMAGE}" \
      org.opencontainers.image.source="https://github.com/mo3789530/hako"

COPY --chmod=0555 hako-actions-runner-guest /usr/local/bin/hako-actions-runner-guest
COPY --chmod=0444 actions-runner-bootstrap.pub /etc/hako/actions-runner-bootstrap.pub

ENV HAKO_ACTIONS_RUNNER_ROOT=/home/runner
EXPOSE 8080
USER runner
ENTRYPOINT ["/usr/local/bin/hako-actions-runner-guest"]
