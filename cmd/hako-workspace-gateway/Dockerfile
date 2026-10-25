# syntax=docker/dockerfile:1
FROM --platform=$BUILDPLATFORM golang:1.27.3-alpine AS build

ARG TARGETOS=linux
ARG TARGETARCH=arm64

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build \
    -trimpath -ldflags="-s -w" -o /out/hako-workspace-gateway ./cmd/hako-workspace-gateway

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build --chown=65532:65532 /out/hako-workspace-gateway /hako-workspace-gateway

USER 65532:65532
EXPOSE 8443
ENTRYPOINT ["/hako-workspace-gateway"]
