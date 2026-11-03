APP     := ecommerce
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: run build build-arm64 test test-db vet fmt tidy migrate migrate-status sqlc

## run: start in development mode (reads .env, templates reload from disk)
run:
	go run ./cmd/web serve

## build: binary for the current OS
build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/$(APP) ./cmd/web

## build-arm64: production binary for the HG680P (Linux ARM64)
build-arm64:
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/$(APP)-linux-arm64 ./cmd/web

test:
	go test ./...

vet:
	go vet ./...

fmt:
	gofmt -w .

tidy:
	go mod tidy

migrate:
	go run ./cmd/web migrate

migrate-status:
	go run ./cmd/web migrate status

## test-db: also run database tests (TEST_DATABASE_URL must point to a test database)
test-db:
	@test -n "$(TEST_DATABASE_URL)" || (echo "set TEST_DATABASE_URL" && exit 1)
	go test ./...

## sqlc: regenerate typed queries (requires the sqlc CLI, v1.31.1)
sqlc:
	sqlc generate
