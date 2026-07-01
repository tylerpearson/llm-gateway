# Add GOPATH/bin so golangci-lint, govulncheck, and goimports are found
# regardless of whether the user has it on their shell PATH.
export PATH := $(shell go env GOPATH)/bin:$(PATH)

BINARY_GATEWAY    := bin/gateway
BINARY_GATEWAYCTL := bin/gatewayctl
COVERAGE_PROFILE  := coverage.out
CONFIG_EXAMPLE    := configs/config.example.yaml
COMPOSE_FILE      := deploy/docker-compose.yml

# Detect Docker Compose: prefer the v2 plugin ("docker compose"), fall back to
# the standalone v1 binary ("docker-compose"). Empty when neither is installed,
# in which case up/down print an install hint instead of a cryptic flag error.
COMPOSE := $(shell \
	if docker compose version >/dev/null 2>&1; then echo "docker compose"; \
	elif command -v docker-compose >/dev/null 2>&1; then echo "docker-compose"; \
	else echo ""; fi)

# Datastore connection strings for host-run tooling (gatewayctl) against the
# docker-compose dev stack. Each falls back to the dev default but is overridden
# by the matching variable already set in your environment.
DEV_MYSQL_DSN      := gateway:gateway@tcp(127.0.0.1:3306)/llmgateway?parseTime=true
DEV_CLICKHOUSE_DSN := clickhouse://127.0.0.1:9000/default
CTL_ENV            := MYSQL_DSN="$${MYSQL_DSN:-$(DEV_MYSQL_DSN)}" CLICKHOUSE_DSN="$${CLICKHOUSE_DSN:-$(DEV_CLICKHOUSE_DSN)}"

.DEFAULT_GOAL := help

.PHONY: help
help: ## List available targets
	@echo "Usage: make <target>"
	@echo ""
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) \
		| awk 'BEGIN {FS = ":.*?## "}; {printf "  %-14s %s\n", $$1, $$2}'

.PHONY: build
build: ## Build gateway and gatewayctl binaries into bin/
	go build -o $(BINARY_GATEWAY) ./cmd/gateway
	go build -o $(BINARY_GATEWAYCTL) ./cmd/gatewayctl

.PHONY: test
test: ## Run tests with race detector
	go test ./... -race

.PHONY: test-cover
test-cover: ## Run tests with coverage profile (coverage.out)
	go test ./... -race -coverprofile=$(COVERAGE_PROFILE) -covermode=atomic
	go tool cover -func=$(COVERAGE_PROFILE)

.PHONY: lint
lint: ## Run golangci-lint (v2)
	golangci-lint run ./...

.PHONY: vuln
vuln: ## Run govulncheck for known vulnerabilities
	govulncheck ./...

.PHONY: vet
vet: ## Run go vet
	go vet ./...

.PHONY: fmt
fmt: ## Format code with gofmt (and goimports when available)
	gofmt -w .
	@which goimports >/dev/null 2>&1 && goimports -w . || true

.PHONY: tidy
tidy: ## Tidy and verify go.mod and go.sum
	go mod tidy

.PHONY: run
run: ## Run the gateway server with the example config
	go run ./cmd/gateway -config $(CONFIG_EXAMPLE)

.PHONY: require-compose
require-compose:
	@if [ -z "$(COMPOSE)" ]; then \
		echo "Docker Compose not found. Install it with:"; \
		echo "  brew install docker-compose"; \
		echo "and ensure a Docker engine is running (Docker Desktop or Colima)."; \
		exit 1; \
	fi

.PHONY: up
up: require-compose ## Start the full stack with Docker Compose
	$(COMPOSE) -f $(COMPOSE_FILE) up

.PHONY: down
down: require-compose ## Stop the Docker Compose stack
	$(COMPOSE) -f $(COMPOSE_FILE) down

.PHONY: migrate
migrate: ## Apply MySQL and ClickHouse schema to the dev stack
	$(CTL_ENV) go run ./cmd/gatewayctl migrate

.PHONY: ctl
ctl: ## Run gatewayctl against the dev stack, e.g. make ctl ARGS="team create acme"
	$(CTL_ENV) go run ./cmd/gatewayctl $(ARGS)

.PHONY: ci
ci: vet lint test vuln ## Full local CI gate (vet, lint, test, vuln)
