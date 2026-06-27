GO ?= go
COMPOSE ?= docker compose -f deploy/docker-compose.yml

# DSN for the gated Postgres integration tests (internal/store/*_test.go). It is the
# same address `make deps` exposes; export TELOS_TEST_DSN to make the gated tests RUN
# instead of t.Skip. test-integration sets it for you.
TELOS_TEST_DSN ?= postgres://telos:telos@localhost:5432/telosmud?sslmode=disable

.DEFAULT_GOAL := help
.PHONY: help up deps down logs test test-race test-integration smoke smoke-twice vet lint build tidy proto migrate migrate-status seed

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

test-integration: ## Run the GATED Postgres integration tests (needs `make deps` up)
	@echo "Running gated integration tests against $(TELOS_TEST_DSN)"
	TELOS_TEST_DSN="$(TELOS_TEST_DSN)" $(GO) test -count=1 ./internal/store/... -run 'TestStorePackRoundTrip|TestImportPackIdempotent|TestCharacterCRUD' -v

smoke: ## Bring up the full docker stack and assert it is healthy + seed exits 0 + a player can look
	./scripts/smoke.sh

smoke-twice: ## Smoke, but bring the stack up TWICE on the same volume (the re-seed/idempotency catch)
	./scripts/smoke.sh --twice

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

migrate: ## Apply DB migrations (embedded goose; uses TELOS_POSTGRES_DSN)
	$(GO) run ./cmd/telos-migrate up

migrate-status: ## Show DB migration status
	$(GO) run ./cmd/telos-migrate status

seed: ## Import the demo content pack into Postgres (pack='demo')
	$(GO) run ./cmd/telos-seed
