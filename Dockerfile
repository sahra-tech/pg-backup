FROM golang:1.27-alpine AS builder

WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download

COPY . .
# Static build: the runtime image has no C toolchain.
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o pg-backup .

FROM alpine:3.24

# postgresql17-client provides pg_dump and pg_dumpall. pg_dumpall locates
# pg_dump via PATH, which the app now inherits correctly from this ENV.
# Alpine 3.24 also ships postgresql18-client; pin the major deliberately, since
# the dump format must match what you intend to restore into.
RUN apk add --no-cache postgresql17-client ca-certificates && \
    pg_dump --version && \
    pg_dumpall --version

ENV PATH="/usr/libexec/postgresql:$PATH"

WORKDIR /app

COPY --from=builder /app/pg-backup .

# Run unprivileged. The mounted backup and log directories must be writable by
# this uid (65532 matches the conventional "nonroot" uid).
RUN adduser -D -u 65532 -h /app nonroot && \
    mkdir -p /app/backups /app/logs && \
    chown -R nonroot:nonroot /app
USER nonroot

# Configuration is read from the environment; see .env.example.
# Use --env-file .env, compose's env_file, or explicit -e flags.
EXPOSE 8080

HEALTHCHECK --interval=30s --timeout=5s --start-period=5s --retries=3 \
    CMD wget -qO- http://127.0.0.1:8080/health || exit 1

CMD ["./pg-backup"]
