FROM golang:1.21-alpine AS builder

WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN go build -o pg-backup .

FROM alpine:latest

# Install PostgreSQL client tools  
RUN apk add --no-cache postgresql17-client ca-certificates && \
    # Ensure tools are properly linked and accessible
    cd /usr/libexec/postgresql && \
    chmod +x pg_dump pg_dumpall && \
    # Test that pg_dumpall can find pg_dump from its directory
    ./pg_dumpall --version && \
    # Verify installation
    which pg_dump && \
    which pg_dumpall && \
    pg_dump --version && \
    pg_dumpall --version

# Add PostgreSQL libexec directory to PATH to ensure pg_dumpall can find pg_dump
ENV PATH="/usr/libexec/postgresql:$PATH"

WORKDIR /app

COPY --from=builder /app/pg-backup .

# Expect config.yaml to be mounted at runtime
# Use volume mount: -v /path/to/config.yaml:/app/config.yaml

CMD ["./pg-backup"]
