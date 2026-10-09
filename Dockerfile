# syntax=docker/dockerfile:1

# ---- Builder (headless server, no CGO => static binary) ----
# Runs natively on the build machine and cross-compiles for the target
# platform, so multi-arch images (amd64 + arm64) don't need slow emulation here.
FROM --platform=$BUILDPLATFORM golang:1.24.13-alpine AS build
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" \
    -o /out/mywhoosh2garmin ./cmd/server

# ---- Runtime ----
FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata
COPY --from=build /out/mywhoosh2garmin /usr/local/bin/mywhoosh2garmin

ARG VERSION=dev
LABEL org.opencontainers.image.title="MyWhoosh2Garmin" \
      org.opencontainers.image.description="Syncs MyWhoosh rides to Garmin Connect (web UI + webhooks)" \
      org.opencontainers.image.source="https://github.com/HauZ22/mywoosh2garmin" \
      org.opencontainers.image.licenses="GPL-3.0-only" \
      org.opencontainers.image.version="${VERSION}"

ENV DATA_DIR=/data \
    HTTP_ADDR=:8080 \
    TZ=Europe/Berlin

VOLUME ["/data"]
EXPOSE 8080

HEALTHCHECK --interval=30s --timeout=5s --start-period=10s \
  CMD ["mywhoosh2garmin", "-healthcheck"]

ENTRYPOINT ["mywhoosh2garmin"]
