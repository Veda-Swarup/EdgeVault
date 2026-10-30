# Makefile for EdgeVault

BINARY_EDGE := bin/edge
BINARY_GC   := bin/gc

# Local dev environment variables (sourced when running `make run`)
export EDGEVAULT_DB_URL              ?= postgres://edgevault:edgevault@localhost:5433/edgevault
export EDGEVAULT_S3_ENDPOINT         ?= http://localhost:9000
export EDGEVAULT_S3_CHUNKS_BUCKET    ?= edgevault-chunks
export EDGEVAULT_S3_SNAPSHOTS_BUCKET ?= edgevault-protected-snapshots
export EDGEVAULT_S3_REGION           ?= us-east-1
export EDGEVAULT_S3_FORCE_PATH_STYLE ?= true
export EDGEVAULT_KMS_FAKE            ?= true
export EDGEVAULT_CHUNK_HMAC_KEY      ?= aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
export EDGEVAULT_LISTEN_ADDR         ?= :8080
export EDGEVAULT_MIGRATIONS_PATH     ?= db/migrations
export AWS_ACCESS_KEY_ID             ?= minioadmin
export AWS_SECRET_ACCESS_KEY         ?= minioadmin

# Test database (separate DB to allow `migrate down` without nuking dev data)
export EDGEVAULT_TEST_DB_URL ?= postgres://edgevault:edgevault@localhost:5433/edgevault_test

.PHONY: all up down build run test test-unit test-integration gc-dry-run tidy lint

all: build

## up: Start PostgreSQL + Silo (MinIO-compatible S3) via docker-compose
up:
	docker compose -f deploy/docker/docker-compose.yml up -d
	@echo "Waiting for services to be healthy..."
	@sleep 3
	@echo "Creating test database if needed..."
	@docker compose -f deploy/docker/docker-compose.yml exec postgres \
	  psql -U edgevault -c "CREATE DATABASE edgevault_test;" 2>/dev/null || true
	@echo ""
	@echo "Stack is up. Run 'make run' to start the edge node."

## down: Stop and remove docker-compose services
down:
	docker compose -f deploy/docker/docker-compose.yml down

## build: Compile both binaries
build:
	go build -o $(BINARY_EDGE) ./cmd/edge
	go build -o $(BINARY_GC) ./cmd/gc

## run: Start the edge node locally (requires 'make up' first)
run:
	go run ./cmd/edge

## test-unit: Run unit tests (no DB required)
test-unit:
	go test ./internal/chunker/... ./internal/crypto/... ./internal/blob/... ./internal/kms/... ./internal/cache/... ./internal/journal/... ./internal/uploader/... ./internal/metrics/... ./internal/detector/... ./internal/meta/... ./internal/gc/... ./cmd/gc/... -v -count=1

## test-integration: Run API integration tests (requires PostgreSQL)
test-integration:
	go test ./internal/api/... -v -count=1

## gc-dry-run: Preview retention and chunk cleanup (requires PostgreSQL + S3)
gc-dry-run:
	go run ./cmd/gc --dry-run

## test: Run all tests
test: test-unit test-integration

## tidy: Tidy go.mod
tidy:
	go mod tidy

## lint: Run go vet
lint:
	go vet ./...
