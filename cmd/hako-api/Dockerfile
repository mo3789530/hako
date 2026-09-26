# syntax=docker/dockerfile:1
FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS build

ARG TARGETOS=linux
ARG TARGETARCH=arm64

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build \
    -trimpath -ldflags="-s -w" -o /out/bootstrap ./cmd/hako-api

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=public.ecr.aws/awsguru/aws-lambda-adapter:1.1.0 /lambda-adapter /opt/extensions/lambda-adapter
COPY --from=build --chown=65532:65532 /out/bootstrap /bootstrap

ENV HAKO_API_HTTP_MODE=true \
    HAKO_API_LISTEN_ADDR=0.0.0.0:8080 \
    AWS_LWA_PORT=8080 \
    AWS_LWA_READINESS_CHECK_PROTOCOL=http \
    AWS_LWA_READINESS_CHECK_PATH=/healthz \
    AWS_LWA_READINESS_CHECK_HEALTHY_STATUS=200-299

EXPOSE 8080
ENTRYPOINT ["/bootstrap"]
