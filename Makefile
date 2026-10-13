.PHONY: help up infra runner down reset logs test test-go test-web e2e build fmt prettier lint preflight preflight-examples

help: ## Show available targets
	@grep -E '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-10s\033[0m %s\n", $$1, $$2}'

up: ## Start Postgres/Keycloak/Redis in Docker and run the API and dashboard natively
	./scripts/start-local.sh

infra: ## Start only Postgres, Keycloak, and Redis
	./scripts/start-local.sh --infra-only

runner: ## Run a local runner from the repo root (official adapters; token from the environment, token file, or prompt)
	./scripts/run-runner.sh

preflight: ## Non-mutating Neon/Upstash preflight (process credentials; no secret output)
	bash ./scripts/preflight.sh

preflight-examples: ## Check checked-in blueprints without network or credentials
	bash ./scripts/preflight.sh --examples

down: ## Stop the apps and containers, keep data
	./scripts/stop-local.sh

reset: ## Stop everything and delete local data
	./scripts/stop-local.sh --reset

logs: ## Follow control plane and container logs
	tail -f tmp/logs/controlplane.log & docker compose --env-file .env -f compose.yaml logs -f --tail 50

test: test-go test-web ## Run Go and web unit tests

test-go: ## Go vet and tests (set TEST_DATABASE_URL for Postgres integration tests)
	go vet ./...
	go test -race ./...

test-web: ## Web typecheck, unit tests, and production build
	cd web && npm ci && npm run typecheck && npm test && npm run build

e2e: ## Browser end-to-end test against the running local stack, including the example runner
	cd e2e && npm ci && npx playwright install chromium && npm test

build: ## Build the CLI and official adapters into dist/
	./scripts/build.sh

fmt: ## Format tracked Go files
	./scripts/fmt.sh

prettier: ## Format the dashboard and end-to-end sources
	./scripts/prettier.sh

lint: ## Check Go formatting, vet, Prettier, and ESLint
	./scripts/lint.sh
