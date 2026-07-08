GO ?= go
COMPOSE ?= docker compose -f deploy/docker-compose.yml

# DSN for the gated Postgres integration tests (tests/integration + the co-located
# internal/store/*_test.go). It is the same address `make deps` exposes; export
# TELOS_TEST_DSN to make the gated tests RUN instead of t.Skip. test-integration sets
# it for you.
TELOS_TEST_DSN ?= postgres://telos:telos@localhost:5432/telosmud?sslmode=disable

# Gate address for the e2e tier (tests/e2e). The dev `make up` stack exposes the gate
# here; override TELOS_E2E_ADDR for a non-default host/port (e.g. a CI compose network).
# The e2e tests SKIP cleanly when this gate is not reachable.
TELOS_E2E_ADDR ?= localhost:4000

# `make loadtest` defaults: synthetic players + how long they drive traffic. Override on the command line,
# e.g. `make loadtest BOTS=1500 LOADTEST_DURATION=2m`.
BOTS ?= 500
LOADTEST_DURATION ?= 30s

.DEFAULT_GOAL := help
.PHONY: help up deps down logs test test-race test-integration test-e2e smoke smoke-twice vet lint build tidy proto migrate migrate-status seed verify verify-full loadtest

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

# verify mirrors the HERMETIC CI jobs (`go` + `lint`) EXACTLY, so "verified" locally means
# the same surface CI checks — not a subset. The gap that let CI sit red for a day was running
# `go test` + the golangci-lint binary but never the gofmt-as-CI-runs check or the lint config
# the action uses. Run this before declaring any slice done; run verify-full before a release-
# shaped milestone (it also exercises the Docker smoke/e2e surface that `go test` cannot).
verify: ## Run the hermetic CI matrix locally (gofmt + buf + vet + build + race tests + lint) — the pre-commit gate
	@echo ">> gofmt (CI-strict: no file may be unformatted)"
	@test -z "$$(gofmt -l . 2>/dev/null)" || { echo "gofmt needed:"; gofmt -l .; exit 1; }
	@command -v buf >/dev/null 2>&1 && { echo ">> buf lint + format (the CI proto job)"; buf lint && buf format --exit-code; } || echo ">> buf not installed — skipping proto checks (CI still runs them)"
	@echo ">> go vet" && $(GO) vet ./...
	@echo ">> go build" && $(GO) build ./...
	@echo ">> go build (release gate: NO dev-autoauth bypass — #96)" && $(GO) build -o /dev/null ./cmd/telos-gate
	@echo ">> go test -race" && $(GO) test ./... -race
	@echo ">> go test -tags telos_devauth (the compiled-in dev-autoauth bypass surface — #96)" && $(GO) test -tags telos_devauth -race ./internal/gate/... ./internal/config/...
	@command -v golangci-lint >/dev/null 2>&1 || { echo "golangci-lint not installed — see https://golangci-lint.run/welcome/install"; exit 1; }
	@echo ">> golangci-lint" && golangci-lint run
	@echo ">> verify OK (hermetic CI surface green)"

test-devauth: ## Run the tests behind the telos_devauth build tag (the compiled-in dev-autoauth bypass; #96)
	$(GO) test -tags telos_devauth -race ./internal/gate/... ./internal/config/...

verify-full: verify ## verify + the Docker smoke/e2e surface CI runs (catches Dockerfile/compose breaks `go test` misses)
	@echo ">> docker smoke (the build + whole-stack surface)" && $(MAKE) smoke-twice
	@echo ">> verify-full OK"

test: ## Run all tests
	$(GO) test ./...

test-race: ## Run all tests (with race)
	$(GO) test -race -count=100 ./...

test-integration: ## Run the GATED Postgres integration tests (needs `make deps` up)
	@echo "Running gated integration tests against $(TELOS_TEST_DSN)"
	TELOS_TEST_DSN="$(TELOS_TEST_DSN)" $(GO) test -count=1 ./tests/integration/... ./internal/store/... -v

test-e2e: ## Run the GATED e2e tier against a live gate (needs `make up`; SKIPs if the gate is down)
	@echo "Running e2e tier against gate $(TELOS_E2E_ADDR)"
	TELOS_E2E_ADDR="$(TELOS_E2E_ADDR)" $(GO) test -tags e2e -count=1 ./tests/e2e/... -v

# Active fuzzing. The seed corpus of every Fuzz* target runs in the normal hermetic `make test`
# (a Fuzz function is also a unit test of its seeds), so per-commit CI already exercises the seeds.
# This target runs the ACTIVE fuzzer (mutation search) for FUZZTIME PER TARGET — slow, so it lives in
# the nightly CI tier, not per-commit. `-run '^$$'` suppresses the package's unit tests so only the
# fuzzer runs. Go fuzzing drives ONE target per invocation, so each is listed explicitly; ADD A LINE
# HERE when you add a Fuzz* function.
FUZZTIME ?= 60s
fuzz: ## Run each fuzz target's ACTIVE fuzzer for FUZZTIME (default 60s; nightly uses longer)
	@set -e; \
	for spec in \
	  "internal/textsan FuzzTextsan" \
	  "internal/telnet  FuzzGMCPSubneg" \
	  "internal/world   FuzzParseTargetSpec" \
	  "internal/world   FuzzDispatch" \
	  "internal/world   FuzzLuaCompile" \
	  "internal/world   FuzzStateJSONRoundTrip" \
	  "internal/world   FuzzFormulaEval" ; do \
	  set -- $$spec; \
	  echo ">> fuzzing $$2 ($(FUZZTIME))"; \
	  $(GO) test ./$$1/ -run '^$$' -fuzz "^$$2$$" -fuzztime $(FUZZTIME) || exit 1; \
	done
	@echo ">> fuzz OK (no new crashers within the time budget)"

# W9 stress/soak. Gated on TELOS_SOAK so the per-commit `go test` skips it; this target sets it and
# cranks the volume. Runs under -race (the churn + concurrent-load burst is where a lifecycle/leak bug
# under concurrency shows). SOAK_CYCLES tunes the churn depth; nightly runs a deep pass.
SOAK_CYCLES ?= 20000
soak: ## Run the W9 stress/soak tier under -race (TELOS_SOAK gated; SOAK_CYCLES tunes depth)
	TELOS_SOAK=1 TELOS_SOAK_CYCLES=$(SOAK_CYCLES) $(GO) test -race -count=1 -run Soak ./internal/world/ -v

loadtest: ## Drive a synthetic-client load run at a gate (override BOTS / LOADTEST_DURATION / TELOS_E2E_ADDR)
	$(GO) run ./cmd/telos-botswarm -addr $(TELOS_E2E_ADDR) -n $(BOTS) -duration $(LOADTEST_DURATION)

smoke: ## Bring up the full docker stack and assert it is healthy + seed exits 0 + a player can look
	./tests/smoke/smoke.sh

smoke-twice: ## Smoke, but bring the stack up TWICE on the same volume (the re-seed/idempotency catch)
	./tests/smoke/smoke.sh --twice

vet: ## go vet
	$(GO) vet ./...

lint: ## golangci-lint run (the project standard; install: https://golangci-lint.run/welcome/install)
	@command -v golangci-lint >/dev/null 2>&1 || { echo "golangci-lint not installed — see https://golangci-lint.run/welcome/install"; exit 1; }
	golangci-lint run

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
