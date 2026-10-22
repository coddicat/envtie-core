# Changelog

All notable changes to EnvTie Core are documented here.

## Unreleased

- Official `sentry@1` adapter: create/delete one Sentry project per environment, return `SENTRY_DSN` and `SENTRY_PROJECT_SLUG`, credential `SENTRY_AUTH_TOKEN`. Hosted control planes need this tag published (binaries + `adapters.json`), then a `go.mod` pin bump and `RUNNER_VERSION` redeploy before consumers can plan `sentry@1`.
- Publish the runner image before attaching its provenance and SBOM, so a new GHCR package is not rejected with `read_package`.
- Added typed adapter runtime inputs and a non-mutating credential preflight. Local runners prompt once for missing values; noninteractive runners leave the approved operation queued.
- Prepared the public CLI, Action, packages, and runner image for `v1.5.0-beta.1`.
- Removed the hosted `preview execute --dispatch` and `preview report` commands.
  Pull-request and custom CI workflows use `preview apply`, `preview close`,
  `preview destroy`, `preview sweep`, and `preview status`.
- Extracted the customer-side CLI, runner, adapters, SDK, blueprint contracts,
  templates, GitHub Action, and packaging into the public `envtie-core`
  repository.
- Added independent PR, security, Docker smoke, and artifact release workflows.
