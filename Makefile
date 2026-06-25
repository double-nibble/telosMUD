GO ?= go
COMPOSE ?= docker compose -f deploy/docker-compose.yml

.DEFAULT_GOAL := help
.PHONY: help up deps down logs test vet lint build tidy proto

help: ## List targets
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | sort | \
		awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-10s\033[0m %s\n", $$1, $$2}'

up: ## Build & start the full stack (deps + world + gate)
	$(COMPOSE) up -d --build

deps: ## Start only the backing services (Postgres, Redis, NATS)
	$(COMPOSE) up -d postgres redis nats

down: ## Stop dev dependencies
	$(COMPOSE) down

logs: ## Tail dev dependency logs
	$(COMPOSE) logs -f

test: ## Run all tests
	$(GO) test ./...

test-race: ## Run all tests (with race)
	$(GO) test -race -count=100 ./...

vet: ## go vet
	$(GO) vet ./...

lint: ## golangci-lint (skips if not installed)
	@command -v golangci-lint >/dev/null 2>&1 && golangci-lint run || echo "golangci-lint not installed; skipping"

build: ## Build all binaries into ./bin
	$(GO) build -o bin/ ./cmd/...

tidy: ## go mod tidy
	$(GO) mod tidy

proto: ## Generate protobuf/gRPC code (requires buf)
	@command -v buf >/dev/null 2>&1 && buf generate || echo "buf not installed; see https://buf.build/docs/installation"
