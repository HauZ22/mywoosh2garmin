# syntax=docker/dockerfile:1

# ---- Builder (headless server, no CGO => static binary) ----
FROM golang:1.24.13-alpine AS build
WORKDIR /src
RUN apk add --no-cache git ca-certificates
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/fittogarmin ./cmd/server

# ---- Runtime ----
FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata
COPY --from=build /out/fittogarmin /usr/local/bin/fittogarmin

ENV DATA_DIR=/data \
    HTTP_ADDR=:8080 \
    TZ=Europe/Berlin

VOLUME ["/data"]
EXPOSE 8080

HEALTHCHECK --interval=30s --timeout=3s --start-period=10s \
  CMD wget -q -O /dev/null http://127.0.0.1:8080/api/status || exit 1

ENTRYPOINT ["fittogarmin"]