# Install ARM64 wheels on an x86-64 builder without requiring binfmt emulation.
FROM --platform=linux/amd64 docker.io/library/python:3.14-slim-bookworm@sha256:1faf2343e747208b2a36e5f2b5ff908ec3b2d0078a6a7bfe73b535698fe83909 AS builder

WORKDIR /app
COPY requirements-runtime.txt ./requirements-runtime.txt
RUN python -m pip install --no-cache-dir --require-hashes --only-binary=:all: \
    --platform manylinux2014_aarch64 --platform manylinux_2_17_aarch64 \
    --platform manylinux_2_28_aarch64 --implementation cp --python-version 3.14 \
    --abi cp314 --target /deps -r requirements-runtime.txt

FROM --platform=linux/arm64 docker.io/library/python:3.14-slim-bookworm@sha256:735903d501abec9d901059dd4ddd52982b0baf881ff7335a803c65c39ff5f4a4

ENV PYTHONDONTWRITEBYTECODE=1 \
    PYTHONUNBUFFERED=1 \
    PIP_DISABLE_PIP_VERSION_CHECK=1 \
    PYTHONPATH=/app/src:/deps

WORKDIR /app
COPY --from=builder /deps /deps
COPY src ./src
USER 10001:10001
EXPOSE 8080
CMD ["python", "-m", "hako_coding_agent.http_runtime"]
