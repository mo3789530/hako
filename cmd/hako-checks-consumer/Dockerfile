# syntax=docker/dockerfile:1
FROM --platform=$BUILDPLATFORM golang:1.27.2-alpine AS build

ARG TARGETOS=linux
ARG TARGETARCH=arm64

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build \
    -trimpath -ldflags="-s -w" -o /out/hako-checks-consumer ./cmd/hako-checks-consumer

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build --chown=65532:65532 /out/hako-checks-consumer /hako-checks-consumer

ENV HAKO_DSQL_USER=hako_checks_consumer
USER 65532:65532
ENTRYPOINT ["/hako-checks-consumer"]
