# Multi-stage build for llm-gateway.
#
# Stage 1 (builder): compiles both binaries with CGO disabled so the output is
# fully static and can run in a distroless image.
# Stage 2 (wget): provides a static wget binary for container healthchecks.
#   busybox:musl ships statically-linked binaries that work in distroless.
# Stage 3 (runtime): minimal distroless image, non-root user, port 8080.
#
# NOTE: distroless has no shell, so the HEALTHCHECK instruction is omitted.
# Healthchecks are defined in deploy/docker-compose.yml and use the static
# wget binary copied in below.

# -------------------------------------------------------------------
# Stage 1: builder
# -------------------------------------------------------------------
FROM golang:1.26 AS builder

WORKDIR /src

# Download dependencies first so they are cached independently of source changes.
COPY go.mod go.sum ./
RUN go mod download

# Copy the rest of the source tree.
COPY . .

# Build the gateway server binary (static, no CGO).
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath -ldflags="-s -w" \
    -o /out/gateway ./cmd/gateway

# Build the admin CLI binary (static, no CGO).
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath -ldflags="-s -w" \
    -o /out/gatewayctl ./cmd/gatewayctl

# -------------------------------------------------------------------
# Stage 2: static wget for healthchecks
# busybox:musl binaries are statically linked, so they run in distroless.
# -------------------------------------------------------------------
FROM busybox:musl AS busybox

# -------------------------------------------------------------------
# Stage 3: runtime (distroless, non-root)
# -------------------------------------------------------------------
FROM gcr.io/distroless/static-debian12:nonroot

WORKDIR /app

# Copy compiled binaries from builder.
COPY --from=builder /out/gateway    /usr/local/bin/gateway
COPY --from=builder /out/gatewayctl /usr/local/bin/gatewayctl

# Copy static wget so compose and orchestrators can run healthchecks without
# needing a shell. Used by: deploy/docker-compose.yml healthcheck stanza.
COPY --from=busybox /bin/wget /usr/local/bin/wget

# Copy the example config as the bundled default.
# Override by mounting a real config at /app/configs/config.yaml at runtime.
COPY configs/config.example.yaml configs/config.yaml

EXPOSE 8080

# The nonroot user (UID 65532) is set by the distroless base image.

CMD ["/usr/local/bin/gateway", "--config", "configs/config.yaml"]
