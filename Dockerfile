# Build stage
FROM golang:1.24-alpine AS builder

WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=linux go build \
    -ldflags="-s -w -X main.Version=${VERSION} -X github.com/anthropics/maestro-mcp/internal/mcp.ServerVersion=${VERSION}" \
    -o maestro ./cmd/maestro

# Runtime stage
FROM alpine:3.19

RUN apk --no-cache add ca-certificates git

# Create non-root user for security.
RUN adduser -D -u 1000 appuser

WORKDIR /app
COPY --from=builder /app/maestro /app/maestro

# Create data directory owned by appuser.
RUN mkdir -p /app/data && chown appuser:appuser /app/data

USER appuser

EXPOSE 8080 3000

ENTRYPOINT ["/app/maestro"]
CMD ["serve", "--db", "/app/data/maestro.db", "--http", ":8080", "--sse", ":3000"]
