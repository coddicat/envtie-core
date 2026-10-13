# Contributor guidance

- Keep this repository safe to publish. Never add `.env` files, credentials,
  hosted-service configuration, customer data, or private implementation.
- Run `gofmt`, `go vet ./...`, and `go test ./...` for Go changes.
- `blueprints/` is the canonical source for YAML templates.
- Keep flags in `scripts/release-binaries.sh` and `Dockerfile.runner` aligned so
  adapter checksums remain reproducible.
- Public contracts in `core`, `plan`, `catalog`, and `sdk/adapter` require
  backwards-compatibility review.
- Release workflows build and publish artifacts only. They must never deploy
  the private hosted service.
