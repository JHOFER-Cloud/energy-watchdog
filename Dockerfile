# syntax=docker/dockerfile:1
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS builder

WORKDIR /build

COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG TARGETOS
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -ldflags="-w -s" -o energy-watchdog .

FROM scratch

# CA certs for HTTPS to the Proxmox/Prometheus/Alertmanager/Kubernetes APIs.
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=builder /build/energy-watchdog /energy-watchdog

EXPOSE 9333

ENTRYPOINT ["/energy-watchdog"]
CMD ["-config", "/config/config.yaml"]
