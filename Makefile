.PHONY: build run test-run list clean docker-build docker-run docker-stop deps test check validate-config help

# Build the application
build:
	go build -o pg-backup .

# Run the application
run: build
	./pg-backup

# Run once for testing
test-run: build
	./pg-backup -once

# List configured databases
list: build
	./pg-backup -list

# Clean build artifacts and logs
clean:
	rm -f pg-backup
	rm -rf backups/
	rm -f backup.log
	rm -rf logs/

# Build Docker image
docker-build:
	docker build -t pg-backup .

# Run with Docker Compose
docker-run:
	docker compose up -d

# Stop Docker containers
docker-stop:
	docker compose down

# Download dependencies
deps:
	go mod tidy
	go mod download

# Run tests (race detector on: the health service is concurrent)
test:
	go test -race ./...

# Everything CI runs
check: build
	gofmt -l . | tee /dev/stderr | (! read)
	go vet ./...
	go test -race ./...

# Validate configuration from .env
validate-config: build
	./pg-backup --env-file .env -list

# Show help
help:
	@echo "Available commands:"
	@echo "  build         - Build the application"
	@echo "  run           - Build and run the application"
	@echo "  test-run      - Run a one-time backup test"
	@echo "  list          - List configured databases"
	@echo "  clean         - Clean build artifacts"
	@echo "  docker-build  - Build Docker image"
	@echo "  docker-run    - Run with Docker Compose"
	@echo "  docker-stop   - Stop Docker containers"
	@echo "  deps          - Download dependencies"
	@echo "  test          - Run tests with the race detector"
	@echo "  check         - Run everything CI runs (fmt, vet, test)"
	@echo "  validate-config - Load .env and list configured databases"
