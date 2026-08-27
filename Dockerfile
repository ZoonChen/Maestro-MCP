# syntax=docker/dockerfile:1.7

# The toolchain versions are deliberately exact and match
# docs/technical/runtime-and-build.md.
FROM node:22.14.0-alpine AS web-builder
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN --mount=type=cache,target=/root/.npm npm ci
COPY web/ ./
RUN npm run build && test -s dist/index.html

FROM golang:1.26.6-alpine AS go-builder
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
COPY --from=web-builder /src/web/dist ./web/dist

ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_TIME=unknown
ARG SOURCE_DATE_EPOCH=0
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    SOURCE_DATE_EPOCH=${SOURCE_DATE_EPOCH} CGO_ENABLED=0 go build -trimpath \
    -ldflags="-s -w \
      -X main.Version=${VERSION} \
      -X main.Commit=${COMMIT} \
      -X main.BuildTime=${BUILD_TIME} \
      -X github.com/ZoonChen/Maestro-MCP/internal/mcp.ServerVersion=${VERSION}" \
    -o /out/maestro ./cmd/maestro
RUN install -d -m 0700 /out/data

# The central M0 server image has no shell, package manager, Git client, or
# writable source mount. Local diagnostic execution is a host-only, explicit
# developer mode; the production Runner sandbox arrives in M1.
FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab

WORKDIR /app
COPY --from=go-builder --chown=10001:10001 /out/maestro /app/maestro
COPY --from=go-builder --chown=10001:10001 /out/data /var/lib/maestro

USER 10001:10001
EXPOSE 8080
VOLUME ["/var/lib/maestro"]

ENTRYPOINT ["/app/maestro"]
CMD ["server", "--db", "/var/lib/maestro/maestro.db", "--http", "0.0.0.0:8080"]
