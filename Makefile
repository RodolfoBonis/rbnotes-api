.DEFAULT_GOAL := help
SHELL := /bin/bash

# Carrega .env se existir (para os alvos locais)
ifneq (,$(wildcard .env))
include .env
export
endif

BIN_DIR := bin
GOBIN := $(shell go env GOPATH)/bin

# Docker (Colima) para os testes de integração (testcontainers).
export DOCKER_HOST ?= unix://$(HOME)/.colima/default/docker.sock
export TESTCONTAINERS_RYUK_DISABLED ?= true

.PHONY: help
help: ## Lista os alvos disponíveis
	@grep -E '^[a-zA-Z0-9_-]+:.*?## .*$$' $(MAKEFILE_LIST) | sort | \
		awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2}'

.PHONY: tools
tools: ## Instala as ferramentas de codegen/lint (goose, sqlc, golangci-lint)
	go install github.com/pressly/goose/v3/cmd/goose@v3.27.1
	go install github.com/sqlc-dev/sqlc/cmd/sqlc@v1.31.1
	go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.12.2

.PHONY: gen
gen: ## Gera as queries tipadas com sqlc
	$(GOBIN)/sqlc generate

.PHONY: build
build: ## Compila os binários (api + ai-worker)
	mkdir -p $(BIN_DIR)
	go build -o $(BIN_DIR)/api       ./cmd/api
	go build -o $(BIN_DIR)/ai-worker ./cmd/ai-worker

.PHONY: run
run: ## Sobe a API localmente
	go run ./cmd/api

.PHONY: run-ai-worker
run-ai-worker: ## Sobe o ai-worker localmente (precisa de CLAUDE_CODE_OAUTH_TOKEN e da CLI `claude`)
	go run ./cmd/ai-worker

.PHONY: test
test: ## Roda todos os testes com -race (integração exige Docker/Colima)
	go test ./... -race -count=1

.PHONY: test-short
test-short: ## Roda só os testes unitários (sem Docker)
	go test ./... -short -race -count=1

.PHONY: smoke
smoke: ## Smoke manual do note_enrich contra a CLI real (precisa de `claude` logado)
	go test -tags manual -run TestRealEnrich -count=1 ./internal/ai/

.PHONY: lint
lint: ## Roda o golangci-lint
	$(GOBIN)/golangci-lint run ./...

.PHONY: openapi-lint
openapi-lint: ## Lint do contrato OpenAPI (Redocly)
	npx --yes @redocly/cli@1.34.2 lint api/openapi.yaml

.PHONY: fmt
fmt: ## Formata e faz vet
	go fmt ./...
	go vet ./...

.PHONY: migrate-up
migrate-up: ## Aplica as migrations
	$(GOBIN)/goose -dir db/migrations postgres "$(DATABASE_URL)" up

.PHONY: migrate-down
migrate-down: ## Reverte a última migration
	$(GOBIN)/goose -dir db/migrations postgres "$(DATABASE_URL)" down

.PHONY: migrate-status
migrate-status: ## Mostra o status das migrations
	$(GOBIN)/goose -dir db/migrations postgres "$(DATABASE_URL)" status

.PHONY: db-up
db-up: ## Sobe Postgres + Redis + MinIO de desenvolvimento
	docker compose up -d

.PHONY: db-down
db-down: ## Derruba os serviços de desenvolvimento
	docker compose down
