.PHONY: help test build fmt vet preflight preflight-examples

help: ## Show available targets
	@grep -E '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-10s\033[0m %s\n", $$1, $$2}'

preflight: ## Non-mutating Neon/Upstash preflight (process credentials; no secret output)
	bash ./scripts/preflight.sh

preflight-examples: ## Check checked-in blueprints without network or credentials
	bash ./scripts/preflight.sh --examples

test: ## Run the public core test suite
	go test -race ./...

build: ## Build the CLI and official adapters into dist/
	./scripts/build.sh

fmt: ## Format tracked Go files
	gofmt -w $$(find . -name '*.go' -not -path './tmp/*')

vet: ## Run Go static checks
	go vet ./...
