# syntax=docker/dockerfile:1

# Build stage: compiled for the target platform on the build machine's own
# architecture, so multi-arch images do not need emulation.
ARG GO_VERSION=1.27
FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-alpine AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

COPY cmd ./cmd
COPY internal ./internal
ARG TARGETOS TARGETARCH
ARG VERSION=dev
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath \
      -ldflags "-s -w -X github.com/biga-sh/server/internal/version.version=${VERSION}" \
      -o /out/biga-server ./cmd/biga-server

# Runtime stage: a static binary on distroless. No shell, no package
# manager, CA certificates for TLS to managed Postgres, and a non-root user.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/biga-server /usr/local/bin/biga-server
USER nonroot:nonroot
ENV BIGA_LISTEN=:8080
EXPOSE 8080
# distroless has no curl; the binary probes itself.
HEALTHCHECK --interval=15s --timeout=5s --start-period=60s --retries=3 \
  CMD ["/usr/local/bin/biga-server", "healthcheck"]
ENTRYPOINT ["/usr/local/bin/biga-server"]
CMD ["serve"]
