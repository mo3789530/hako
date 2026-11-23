# AgentCore Runtime runs Linux/ARM64 images. Override only with another digest-pinned image.
FROM --platform=linux/arm64 docker.io/library/python:3.14-slim-bookworm@sha256:735903d501abec9d901059dd4ddd52982b0baf881ff7335a803c65c39ff5f4a4

ENV PYTHONDONTWRITEBYTECODE=1 \
    PYTHONUNBUFFERED=1 \
    PIP_DISABLE_PIP_VERSION_CHECK=1 \
    PYTHONPATH=/app/src

WORKDIR /app
COPY requirements-runtime.txt ./requirements-runtime.txt
RUN python -m pip install --no-cache-dir --require-hashes -r requirements-runtime.txt

COPY src ./src
USER 10001:10001
EXPOSE 8080
CMD ["python", "-m", "hako_coding_agent.http_runtime"]
