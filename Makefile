# Add GOPATH/bin so golangci-lint, govulncheck, and goimports are found
# regardless of whether the user has it on their shell PATH.
export PATH := $(shell go env GOPATH)/bin:$(PATH)

BINARY_GATEWAY    := bin/gateway
BINARY_GATEWAYCTL := bin/gatewayctl
COVERAGE_PROFILE  := coverage.out
CONFIG_EXAMPLE    := configs/config.example.yaml
COMPOSE_FILE      := deploy/docker-compose.yml

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

.PHONY: up
up: ## Start the full stack with Docker Compose
	docker compose -f $(COMPOSE_FILE) up

.PHONY: down
down: ## Stop the Docker Compose stack
	docker compose -f $(COMPOSE_FILE) down

.PHONY: ci
ci: vet lint test vuln ## Full local CI gate (vet, lint, test, vuln)
