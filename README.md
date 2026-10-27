# EnvTie Core

[![CI](https://github.com/coddicat/envtie-core/actions/workflows/ci.yml/badge.svg)](https://github.com/coddicat/envtie-core/actions/workflows/ci.yml)

Public customer-side components and contracts for [EnvTie.dev](https://envtie.dev):

> Public beta: pin `v1.5.0-beta.1` explicitly. Prerelease interfaces may change
> before general availability.

- `envtie` CLI and workspace runner
- official Neon, Upstash, Stripe, and GitHub environment adapters
- Go adapter SDK and protocol
- blueprint model, schema, and canonical YAML templates
- GitHub Action and `@envtie.dev/cli` launcher
- `envtie-runner` container image

The hosted control plane, dashboard, identity, billing, database, and deployment
configuration live in a separate private repository. This repository never
contains customer credentials.

## Install

```sh
npm install --save-dev @envtie.dev/cli
npx envtie --help
```

Or run a long-lived workspace runner:

```sh
docker run --rm \
  -e ENVTIE_API_URL=https://envtie.dev \
  -e ENVTIE_RUNNER_TOKEN \
  ghcr.io/coddicat/envtie-runner:1.5.0-beta.1
```

Runner tokens and provider credentials should be supplied by a secret manager.
Do not commit them to blueprint files.

## Blueprints

Canonical templates are in [`blueprints/`](blueprints/), and the JSON Schema is
[`schemas/blueprint.schema.json`](schemas/blueprint.schema.json). Start with a
template and replace only its explicit placeholders.

Official adapters include Neon, Upstash, Stripe, GitHub, Sentry, Mailtrap, and
Vercel (`vercel@1` — branch preview deploy + env vars on an existing project).
Stripe (`stripe@1`) registers one webhook per environment; `host` is a public
hostname or URL. Pair with Vercel via `host: "{host.VERCEL_URL}"` and
`depends_on: [host]`, or use your own preview DNS such as
`{env}.preview.example.com` (see [`blueprints/stripe-webhook.yaml`](blueprints/stripe-webhook.yaml)).
Custom providers can implement the protocol in [`sdk/adapter`](sdk/adapter); see
[`examples/custom-adapter`](examples/custom-adapter) and `envtie adapter init`.
`envtie help` summarizes runner env vars and the Stripe/Vercel host pattern.

## Development

Requires Go 1.27.2 and Node.js 22.18.0.

```sh
go test ./...
go vet ./...
node --check packages/cli/bin/envtie.js
docker build -f Dockerfile.runner .
```

## Releases

Release tags build CLI and adapter binaries once, publish checksums and
`adapters.json`, publish npm packages with provenance, and publish the
multi-platform runner image with SBOM and provenance. Releases do not deploy
the hosted EnvTie.dev service.

See [`RELEASING.md`](RELEASING.md) for the release and compatibility contract.

## Security

See [`SECURITY.md`](SECURITY.md). Never report vulnerabilities in a public
issue.

## License

Apache-2.0.
