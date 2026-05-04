SHELL := bash
.SHELLFLAGS := -eu -o pipefail -c
.DEFAULT_GOAL := help

# Load .env into every recipe when present. Missing on first clone is fine;
# the API surfaces a clear "missing required env vars" error at startup.
ifneq (,$(wildcard .env))
    include .env
    export
endif

.PHONY: help db-up db-down migrate test test-integration lint run dev

help: ## Show this help.
	@awk 'BEGIN {FS = ":.*?## "}; /^[a-zA-Z0-9_.-]+:.*?## / {printf "  \033[1m%-18s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST)

db-up: ## Start the local Postgres container, wait until healthy.
	docker compose up -d --wait db

db-down: ## Stop the Postgres container.
	docker compose down

migrate: ## Apply all pending migrations.
	tern migrate -m db/migrations

test: ## Run the fast unit tests (no Docker).
	go test ./...

test-integration: ## Run unit + integration tests (requires Docker).
	go test -tags integration ./...

lint: ## Run golangci-lint (config: .golangci.yml; requires v2.x).
	golangci-lint run

run: ## Run the API against the local Postgres.
	go run ./cmd/api

dev: db-up migrate run ## Bootstrap everything and start the API.
